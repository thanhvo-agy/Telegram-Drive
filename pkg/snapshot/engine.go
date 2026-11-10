package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/crypto"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/telegram"
)

// ProgressEvent reports live state during backup execution
type ProgressEvent struct {
	TotalFiles        int     `json:"total_files"`
	ProcessedFiles    int     `json:"processed_files"`
	CurrentFile       string  `json:"current_file"`
	TotalBytes        int64   `json:"total_bytes"`
	UploadedBytes     int64   `json:"uploaded_bytes"`
	DeduplicatedBytes int64   `json:"deduplicated_bytes"`
	CurrentSpeedBps   float64 `json:"current_speed_bps"`
	PercentComplete   float64 `json:"percent_complete"`
	IsComplete        bool    `json:"is_complete"`
	Error             string  `json:"error,omitempty"`
}

// ProgressCallback is called as files and chunks are processed
type ProgressCallback func(p ProgressEvent)

// Engine orchestrates Time Machine snapshots, delta deduplication, and Telegram synchronization
type Engine struct {
	cfg            *config.Config
	tgClient       *telegram.Client
	stateDir       string
	mu             sync.RWMutex
	lastState      *ProgressEvent
	remoteTimeFunc func(ctx context.Context) (time.Time, error)
}

// NewEngine initializes the snapshot engine
func NewEngine(cfg *config.Config, tgClient *telegram.Client) (*Engine, error) {
	stateDir := ""
	if cfg != nil {
		stateDir = cfg.StateDir
	}
	if stateDir == "" {
		if envDir := os.Getenv("TGDRIVE_STATE_DIR"); envDir != "" {
			stateDir = envDir
		} else if cfg != nil && cfg.ConfigFilePath != "" && filepath.Dir(cfg.ConfigFilePath) != "." {
			candidate := filepath.Join(filepath.Dir(cfg.ConfigFilePath), "snapshots")
			if _, err := os.Stat(candidate); err == nil {
				stateDir = candidate
			}
		}
	}
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		stateDir = filepath.Join(home, ".config", "tgdrive", "snapshots")
	}

	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	return &Engine{
		cfg:      cfg,
		tgClient: tgClient,
		stateDir: stateDir,
	}, nil
}

// StateDir returns the path to the local snapshot manifests directory
func (e *Engine) StateDir() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.stateDir
}

// SetStateDir updates the directory where snapshot manifests are stored
func (e *Engine) SetStateDir(dir string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stateDir = dir
}

// Config returns the configuration associated with this engine
func (e *Engine) Config() *config.Config {
	return e.cfg
}

// verifyManifestIntegrity validates cryptographic HMAC if WORM is enabled or HMAC is present
func (e *Engine) verifyManifestIntegrity(m *SnapshotManifest) {
	if m == nil || e.cfg == nil || e.cfg.EncryptionKey == "" {
		return
	}
	if m.WORM.ManifestHMAC != "" || m.WORM.Enabled || (e.cfg.WORM.Enabled) {
		if m.WORM.ManifestHMAC == "" || !m.VerifyHMAC(e.cfg.EncryptionKey) {
			m.Tampered = true
			fmt.Fprintf(os.Stderr, "[SECURITY ALERT] Snapshot %s failed HMAC-SHA256 signature verification: marked TAMPERED\n", m.SnapshotID)
		}
	}
}

// SetRemoteTimeFunc sets a custom function to fetch remote time for clock skew verification (used in tests or custom NTP)
func (e *Engine) SetRemoteTimeFunc(fn func(ctx context.Context) (time.Time, error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.remoteTimeFunc = fn
}

// FetchRemoteTime queries the configured remote source (custom func or Telegram API) to get a trusted remote timestamp
func (e *Engine) FetchRemoteTime(ctx context.Context) (time.Time, error) {
	e.mu.RLock()
	fn := e.remoteTimeFunc
	e.mu.RUnlock()
	if fn != nil {
		return fn(ctx)
	}

	apiBase := os.Getenv("TELEGRAM_API_URL")
	if apiBase == "" {
		apiBase = "https://api.telegram.org"
	}

	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "HEAD", apiBase, nil)
	if err != nil {
		return time.Time{}, err
	}

	httpClient := &http.Client{Timeout: 3 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Fallback to GET if HEAD failed
		req, err = http.NewRequestWithContext(reqCtx, "GET", apiBase, nil)
		if err != nil {
			return time.Time{}, err
		}
		resp, err = httpClient.Do(req)
		if err != nil {
			return time.Time{}, err
		}
	}
	defer resp.Body.Close()

	dateHeader := resp.Header.Get("Date")
	if dateHeader == "" {
		return time.Time{}, errors.New("no HTTP Date header in remote response")
	}

	return time.Parse(time.RFC1123, dateHeader)
}

// ListSnapshots returns all stored snapshots sorted newest to oldest
func (e *Engine) ListSnapshots() ([]SnapshotSummary, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	entries, err := os.ReadDir(e.stateDir)
	if err != nil {
		return nil, err
	}

	var list []SnapshotSummary
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") && strings.HasPrefix(entry.Name(), "snap-") {
			filePath := filepath.Join(e.stateDir, entry.Name())
			data, err := os.ReadFile(filePath)
			if err != nil {
				continue
			}
			var m SnapshotManifest
			if err := json.Unmarshal(data, &m); err == nil {
				e.verifyManifestIntegrity(&m)
				sum := m.Summary()
				sum.Tampered = m.Tampered
				list = append(list, sum)
			}
		}
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})

	return list, nil
}

// GetSnapshot reads a full manifest by SnapshotID and verifies integrity
func (e *Engine) GetSnapshot(id string) (*SnapshotManifest, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	filePath := filepath.Join(e.stateDir, id+".json")
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read snapshot %s: %w", id, err)
	}

	var m SnapshotManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", id, err)
	}
	e.verifyManifestIntegrity(&m)
	return &m, nil
}

// LoadSnapshot reads a snapshot manifest and returns ErrTamperedManifest if altered
func (e *Engine) LoadSnapshot(id string) (*SnapshotManifest, error) {
	m, err := e.GetSnapshot(id)
	if err != nil {
		return nil, err
	}
	if m.Tampered {
		return m, ErrTamperedManifest
	}
	return m, nil
}

// GetLatestSnapshot returns the most recent completed snapshot
func (e *Engine) GetLatestSnapshot() (*SnapshotManifest, error) {
	list, err := e.ListSnapshots()
	if err != nil || len(list) == 0 {
		return nil, nil
	}
	return e.GetSnapshot(list[0].SnapshotID)
}

// FileVersion represents a single historical version of a file across snapshots
type FileVersion struct {
	SnapshotID string    `json:"snapshot_id"`
	CreatedAt  time.Time `json:"created_at"`
	Size       int64     `json:"size"`
	ModTime    int64     `json:"mod_time"`
	FileHash   string    `json:"file_hash"`
	ChunkCount int       `json:"chunk_count"`
}

// GetFileHistory retrieves all historical versions of a specific file path across all snapshots
func (e *Engine) GetFileHistory(relPath string) ([]FileVersion, error) {
	summaries, err := e.ListSnapshots()
	if err != nil {
		return nil, err
	}

	var versions []FileVersion
	for _, sum := range summaries {
		m, err := e.GetSnapshot(sum.SnapshotID)
		if err != nil {
			continue
		}
		if entry, exists := m.Files[relPath]; exists {
			versions = append(versions, FileVersion{
				SnapshotID: m.SnapshotID,
				CreatedAt:  m.CreatedAt,
				Size:       entry.Size,
				ModTime:    entry.ModTime,
				FileHash:   entry.FileHash,
				ChunkCount: len(entry.Chunks),
			})
		}
	}
	return versions, nil
}

// shouldExclude checks if relative path matches any exclusion patterns
func (e *Engine) shouldExclude(relPath string) bool {
	for _, pattern := range e.cfg.Excludes {
		if strings.HasSuffix(pattern, "/**") {
			prefix := strings.TrimSuffix(pattern, "/**")
			if strings.HasPrefix(relPath, prefix) || strings.Contains(relPath, "/"+prefix+"/") || strings.Contains(relPath, "/"+prefix) {
				return true
			}
		} else if strings.HasPrefix(pattern, "*.") {
			ext := strings.TrimPrefix(pattern, "*")
			if strings.HasSuffix(relPath, ext) {
				return true
			}
		} else {
			if matched, _ := filepath.Match(pattern, filepath.Base(relPath)); matched {
				return true
			}
		}
	}
	return false
}

// CreateSnapshot scans source directory, performs CoW deduplication, encrypts and uploads changed files
func (e *Engine) CreateSnapshot(ctx context.Context, cb ProgressCallback) (*SnapshotManifest, error) {
	if e.tgClient == nil {
		return nil, errors.New("telegram client is not configured: please set bot_tokens and channel_id")
	}

	startTime := time.Now()
	prevManifest, _ := e.GetLatestSnapshot()

	sourceDir := e.cfg.SourcePath
	if _, err := os.Stat(sourceDir); err != nil {
		return nil, fmt.Errorf("source path does not exist: %w", err)
	}

	// 1. Scan filesystem
	type scanItem struct {
		absPath string
		relPath string
		size    int64
		modTime int64
	}

	var fileList []scanItem
	var dirList []string
	var totalSourceBytes int64

	err := filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip unreadable paths
		}
		if path == sourceDir {
			return nil
		}

		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if e.shouldExclude(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if info.IsDir() {
			dirList = append(dirList, rel)
			return nil
		}

		fileList = append(fileList, scanItem{
			absPath: path,
			relPath: rel,
			size:    info.Size(),
			modTime: info.ModTime().Unix(),
		})
		totalSourceBytes += info.Size()
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walk source directory: %w", err)
	}

	snapID := fmt.Sprintf("snap-%s", startTime.Format("20060102-150405"))
	newManifest := &SnapshotManifest{
		SnapshotID:  snapID,
		CreatedAt:   startTime,
		NodeName:    e.cfg.NodeName,
		SourcePath:  sourceDir,
		TotalFiles:  len(fileList),
		TotalBytes:  totalSourceBytes,
		Directories: dirList,
		Files:       make(map[string]FileEntry),
	}

	prog := ProgressEvent{
		TotalFiles: len(fileList),
		TotalBytes: totalSourceBytes,
	}

	chunkSize := e.cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 49 * 1024 * 1024
	}

	// 2. Process each file with CoW Delta check
	for idx, item := range fileList {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		prog.ProcessedFiles = idx + 1
		prog.CurrentFile = item.relPath
		if prog.TotalBytes > 0 {
			prog.PercentComplete = float64(prog.UploadedBytes+prog.DeduplicatedBytes) / float64(prog.TotalBytes) * 100.0
		}
		if cb != nil {
			cb(prog)
		}

		// Check if file can be deduplicated from previous snapshot
		if prevManifest != nil {
			if prevEntry, exists := prevManifest.Files[item.relPath]; exists {
				if prevEntry.Size == item.size && prevEntry.ModTime == item.modTime {
					// 100% matched: CoW deduplicated! 0 bytes uploaded!
					newManifest.Files[item.relPath] = prevEntry
					prog.DeduplicatedBytes += item.size
					newManifest.DeduplicatedBytes += item.size
					continue
				}
			}
		}

		// New or modified file: chunk, encrypt, and upload
		entry, uploadedBytes, err := e.uploadFile(ctx, item.absPath, item.relPath, item.size, item.modTime, chunkSize)
		if err != nil {
			return nil, fmt.Errorf("upload %s: %w", item.relPath, err)
		}

		newManifest.Files[item.relPath] = *entry
		prog.UploadedBytes += uploadedBytes
		newManifest.UploadedBytes += uploadedBytes
	}

	newManifest.DurationSec = time.Since(startTime).Seconds()

	// WORM Metadata & Cryptographic HMAC Manifest Signing
	if e.cfg != nil && e.cfg.WORM.Enabled {
		lockedAt := startTime
		mode := WORMMode(e.cfg.WORM.Mode)
		if mode == "" {
			mode = WORMModeCompliance
		}
		retentionDays := e.cfg.WORM.RetentionDays
		if retentionDays <= 0 && mode == WORMModeCompliance {
			retentionDays = 30
		}
		retainUntil := lockedAt.Add(time.Duration(retentionDays) * 24 * time.Hour)
		newManifest.WORM = WORMMetadata{
			Enabled:       true,
			Mode:          mode,
			LockedAt:      lockedAt,
			RetainUntil:   retainUntil,
			RetentionDays: retentionDays,
			LegalHold:     e.cfg.WORM.LegalHold,
		}
	}
	if e.cfg != nil && e.cfg.EncryptionKey != "" {
		newManifest.WORM.ManifestHMAC = newManifest.ComputeHMAC(e.cfg.EncryptionKey)
	}

	// 3. Save local manifest
	manifestJSON, err := json.MarshalIndent(newManifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}

	localPath := filepath.Join(e.StateDir(), snapID+".json")
	if err := os.WriteFile(localPath, manifestJSON, 0640); err != nil {
		return nil, fmt.Errorf("save local manifest: %w", err)
	}

	// 4. Encrypt and upload manifest to Telegram
	encManifest, err := newManifest.Encrypt(e.cfg.EncryptionKey)
	if err == nil && e.tgClient != nil {
		caption := fmt.Sprintf("[TCDRIVE_MANIFEST] %s | Files: %d | Size: %d", snapID, newManifest.TotalFiles, newManifest.TotalBytes)
		doc, _, err := e.tgClient.UploadDocument(ctx, snapID+".manifest.enc", encManifest, caption)
		if err == nil && doc != nil {
			newManifest.TelegramManifestID = doc.FileID
			// Update local manifest with Telegram manifest ID
			if updatedJSON, err := json.MarshalIndent(newManifest, "", "  "); err == nil {
				_ = os.WriteFile(localPath, updatedJSON, 0640)
			}
		}
	}

	// 5. Send Telegram alert notification
	alertMsg := fmt.Sprintf("*Snapshot Created: %s*\n"+
		"Node: `%s`\n"+
		"Total Files: `%d`\n"+
		"Total Size: `%.2f MB`\n"+
		"Uploaded Delta: `%.2f MB`\n"+
		"Deduplicated (CoW): `%.2f MB`\n"+
		"Duration: `%.1fs`",
		snapID,
		newManifest.NodeName,
		newManifest.TotalFiles,
		float64(newManifest.TotalBytes)/(1024*1024),
		float64(newManifest.UploadedBytes)/(1024*1024),
		float64(newManifest.DeduplicatedBytes)/(1024*1024),
		newManifest.DurationSec,
	)
	_ = e.tgClient.SendAlert(ctx, alertMsg)

	prog.IsComplete = true
	prog.PercentComplete = 100.0
	if cb != nil {
		cb(prog)
	}

	return newManifest, nil
}

func (e *Engine) uploadFile(ctx context.Context, absPath, relPath string, size, modTime int64, chunkSize int64) (*FileEntry, int64, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	entry := &FileEntry{
		Path:    relPath,
		Size:    size,
		ModTime: modTime,
	}

	var uploadedDelta int64
	var chunkIdx int
	var offset int64
	buf := make([]byte, chunkSize)

	for {
		n, err := io.ReadFull(f, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, 0, err
		}
		if n == 0 {
			break
		}

		plainChunk := buf[:n]
		chunkHash := crypto.HashData(plainChunk)

		// Encrypt chunk with AES-256-GCM
		encChunk, err := crypto.EncryptChunk(plainChunk, e.cfg.EncryptionKey)
		if err != nil {
			return nil, 0, fmt.Errorf("encrypt chunk %d: %w", chunkIdx, err)
		}

		chunkFilename := fmt.Sprintf("%s.chunk.%04d.enc", filepath.Base(relPath), chunkIdx)
		caption := fmt.Sprintf("[CHUNK] %s part %d", relPath, chunkIdx)

		doc, msgID, err := e.tgClient.UploadDocument(ctx, chunkFilename, encChunk, caption)
		if err != nil {
			return nil, 0, fmt.Errorf("upload chunk %d: %w", chunkIdx, err)
		}

		entry.Chunks = append(entry.Chunks, ChunkRef{
			Index:          chunkIdx,
			Offset:         offset,
			Size:           int64(n),
			EncryptedSize:  int64(len(encChunk)),
			Hash:           chunkHash,
			TelegramFileID: doc.FileID,
			TelegramMsgID:  msgID,
		})

		uploadedDelta += int64(len(encChunk))
		offset += int64(n)
		chunkIdx++

		if n < int(chunkSize) {
			break
		}
	}

	// Calculate overall file hash
	if _, err := f.Seek(0, 0); err == nil {
		hash, _, _ := crypto.HashStream(f)
		entry.FileHash = hash
	}

	return entry, uploadedDelta, nil
}

// PruneSnapshots applies the configured retention policy and removes expired snapshots
func (e *Engine) PruneSnapshots() (PrunePlan, error) {
	snapshots, err := e.ListSnapshots()
	if err != nil {
		return PrunePlan{}, err
	}

	// Security Quarantine: If ANY snapshot is tampered, abort all pruning immediately
	for _, s := range snapshots {
		if s.Tampered {
			return PrunePlan{}, fmt.Errorf("security alert: tampered snapshot detected (%s); aborting all prune operations to prevent ransomware data destruction", s.SnapshotID)
		}
	}

	now := time.Now()

	// Clock Skew Guard 1: Verify host system clock has not moved backwards relative to newest snapshot
	if len(snapshots) > 0 {
		newest := snapshots[0]
		if now.Before(newest.CreatedAt) {
			return PrunePlan{}, fmt.Errorf("security alert: detected system clock skew backwards (system time %v is before newest snapshot %s created at %v); aborting prune operations", now.UTC(), newest.SnapshotID, newest.CreatedAt.UTC())
		}
	}

	// Clock Skew Guard 2: Verify host system clock against trusted remote time source if available
	if remoteTime, rErr := e.FetchRemoteTime(context.Background()); rErr == nil && !remoteTime.IsZero() {
		skewed, skew := CheckClockSkew(now, remoteTime, 1*time.Hour)
		if skewed {
			fmt.Fprintf(os.Stderr, "[SECURITY ALERT] Clock skew detected between host (%v) and remote (%v): skew %v > 1h; aborting prune\n", now.UTC(), remoteTime.UTC(), skew)
			return PrunePlan{}, fmt.Errorf("security alert: clock skew detected between host and remote (%v > 1h); aborting prune operations", skew)
		}
	}

	plan := CalculatePrunePlan(snapshots, e.cfg.Retention)

	// WORM Lock Guard: Validate that no WORM-locked snapshots are included in deletion list
	for _, del := range plan.Delete {
		if del.IsWORMLocked(now) {
			return PrunePlan{}, fmt.Errorf("%w: %s", ErrSnapshotWORMLocked, del.SnapshotID)
		}
		if m, err := e.GetSnapshot(del.SnapshotID); err == nil && m != nil {
			if m.IsWORMLocked(now) {
				return PrunePlan{}, fmt.Errorf("%w: %s", ErrSnapshotWORMLocked, del.SnapshotID)
			}
		}
	}

	for _, del := range plan.Delete {
		filePath := filepath.Join(e.StateDir(), del.SnapshotID+".json")
		_ = os.Remove(filePath)
	}

	return plan, nil
}

// DeleteSnapshot removes a specific snapshot manifest if not protected by WORM or tampered
func (e *Engine) DeleteSnapshot(id string) error {
	m, err := e.GetSnapshot(id)
	if err != nil {
		return err
	}
	now := time.Now()
	if m.Tampered || m.IsWORMLocked(now) {
		return fmt.Errorf("%w: %s", ErrSnapshotWORMLocked, id)
	}
	filePath := filepath.Join(e.StateDir(), id+".json")
	return os.Remove(filePath)
}

// ReindexSummary reports the results of a disaster recovery reindex operation
type ReindexSummary struct {
	ScannedMessages    int   `json:"scanned_messages"`
	ManifestsFound     int   `json:"manifests_found"`
	ManifestsRestored  int   `json:"manifests_restored"`
	ManifestsSkipped   int   `json:"manifests_skipped"`
	TotalBytesRestored int64 `json:"total_bytes_restored"`
	TotalFilesRestored int   `json:"total_files_restored"`
}

type tgUpdateItem struct {
	UpdateID    int64      `json:"update_id"`
	Message     *tgMsgItem `json:"message,omitempty"`
	ChannelPost *tgMsgItem `json:"channel_post,omitempty"`
}

type tgMsgItem struct {
	MessageID int64      `json:"message_id"`
	Caption   string     `json:"caption,omitempty"`
	Document  *tgDocItem `json:"document,omitempty"`
}

type tgDocItem struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// ReindexFromTelegram performs disaster recovery by scanning Telegram for manifests and reconstructing local index
func (e *Engine) ReindexFromTelegram(ctx context.Context) (*ReindexSummary, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	summary := &ReindexSummary{}

	if err := os.MkdirAll(e.stateDir, 0750); err != nil {
		return summary, fmt.Errorf("create state dir: %w", err)
	}

	if e.cfg == nil || len(e.cfg.BotTokens) == 0 {
		return summary, errors.New("no telegram bot tokens configured for reindexing")
	}

	apiBase := os.Getenv("TELEGRAM_API_URL")
	if apiBase == "" {
		apiBase = "https://api.telegram.org"
	}

	httpClient := &http.Client{Timeout: 60 * time.Second}
	restoredSnapshots := make(map[string]bool)

	// Scan through each bot token in mesh
	for _, tok := range e.cfg.BotTokens {
		var offset int64 = 0
		for {
			select {
			case <-ctx.Done():
				return summary, ctx.Err()
			default:
			}

			getURL := fmt.Sprintf("%s/bot%s/getUpdates?offset=%d&limit=100&timeout=0", apiBase, tok, offset)
			req, err := http.NewRequestWithContext(ctx, "GET", getURL, nil)
			if err != nil {
				return summary, fmt.Errorf("create getUpdates request: %w", err)
			}

			resp, err := httpClient.Do(req)
			if err != nil {
				return summary, fmt.Errorf("telegram getUpdates network error: %w", err)
			}

			bodyBytes, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return summary, fmt.Errorf("read getUpdates response: %w", err)
			}
			if resp.StatusCode != http.StatusOK {
				return summary, fmt.Errorf("telegram getUpdates failed with HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
			}

			var apiResp struct {
				OK          bool           `json:"ok"`
				Description string         `json:"description,omitempty"`
				ErrorCode   int            `json:"error_code,omitempty"`
				Result      []tgUpdateItem `json:"result"`
			}
			if err := json.Unmarshal(bodyBytes, &apiResp); err != nil {
				return summary, fmt.Errorf("unmarshal getUpdates response: %w", err)
			}
			if !apiResp.OK {
				return summary, fmt.Errorf("telegram getUpdates API error (%d): %s", apiResp.ErrorCode, apiResp.Description)
			}

			if len(apiResp.Result) == 0 {
				break
			}

			advanced := false
			for _, up := range apiResp.Result {
				if up.UpdateID >= offset {
					offset = up.UpdateID + 1
					advanced = true
				}
				summary.ScannedMessages++

				msg := up.Message
				if msg == nil {
					msg = up.ChannelPost
				}
				if msg == nil || msg.Document == nil {
					continue
				}

				if !strings.Contains(msg.Caption, "[TCDRIVE_MANIFEST]") {
					continue
				}

				summary.ManifestsFound++

				// Download manifest document
				var manifestData []byte
				if e.tgClient != nil {
					manifestData, err = e.tgClient.DownloadFile(ctx, msg.Document.FileID)
				} else {
					manifestData, err = e.downloadDirect(ctx, httpClient, apiBase, tok, msg.Document.FileID)
				}

				if err != nil {
					summary.ManifestsSkipped++
					continue
				}

				// Attempt decryption
				var manifest *SnapshotManifest
				manifest, err = DecryptManifest(manifestData, e.cfg.EncryptionKey)
				if err != nil {
					// Fallback to unencrypted JSON parsing
					var rawManifest SnapshotManifest
					if uErr := json.Unmarshal(manifestData, &rawManifest); uErr == nil {
						manifest = &rawManifest
					} else {
						summary.ManifestsSkipped++
						continue
					}
				}

				if manifest == nil || manifest.SnapshotID == "" {
					summary.ManifestsSkipped++
					continue
				}

				// Verify cryptographic HMAC and WORM integrity
				if (manifest.WORM.ManifestHMAC != "" || manifest.WORM.Enabled || (e.cfg != nil && e.cfg.WORM.Enabled)) && e.cfg != nil && e.cfg.EncryptionKey != "" {
					if manifest.WORM.ManifestHMAC == "" || !manifest.VerifyHMAC(e.cfg.EncryptionKey) {
						manifest.Tampered = true
						summary.ManifestsSkipped++
						fmt.Fprintf(os.Stderr, "[SECURITY ALERT] Remote manifest %s failed HMAC verification: skipped\n", manifest.SnapshotID)
						continue
					}
				}

				if restoredSnapshots[manifest.SnapshotID] {
					continue
				}

				// Save reconstructed manifest to local stateDir
				localFile := filepath.Join(e.stateDir, manifest.SnapshotID+".json")
				jsonData, err := json.MarshalIndent(manifest, "", "  ")
				if err != nil {
					summary.ManifestsSkipped++
					continue
				}

				if err := os.WriteFile(localFile, jsonData, 0640); err != nil {
					summary.ManifestsSkipped++
					continue
				}

				restoredSnapshots[manifest.SnapshotID] = true
				summary.ManifestsRestored++
				summary.TotalFilesRestored += manifest.TotalFiles
				summary.TotalBytesRestored += manifest.TotalBytes
			}

			if !advanced {
				break
			}
		}
	}

	return summary, nil
}

func (e *Engine) downloadDirect(ctx context.Context, client *http.Client, apiBase, token, fileID string) ([]byte, error) {
	getURL := fmt.Sprintf("%s/bot%s/getFile?file_id=%s", apiBase, token, fileID)
	req, err := http.NewRequestWithContext(ctx, "GET", getURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var getFileResp struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&getFileResp); err != nil || !getFileResp.OK {
		return nil, errors.New("getFile failed")
	}

	downloadURL := fmt.Sprintf("%s/file/bot%s/%s", apiBase, token, getFileResp.Result.FilePath)
	dReq, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return nil, err
	}
	dResp, err := client.Do(dReq)
	if err != nil {
		return nil, err
	}
	defer dResp.Body.Close()

	if dResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download HTTP status %d", dResp.StatusCode)
	}

	return io.ReadAll(dResp.Body)
}
