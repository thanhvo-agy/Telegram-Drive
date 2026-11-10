package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	cfg       *config.Config
	tgClient  *telegram.Client
	stateDir  string
	mu        sync.RWMutex
	lastState *ProgressEvent
}

// NewEngine initializes the snapshot engine
func NewEngine(cfg *config.Config, tgClient *telegram.Client) (*Engine, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}

	stateDir := filepath.Join(home, ".config", "tgdrive", "snapshots")
	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	return &Engine{
		cfg:      cfg,
		tgClient: tgClient,
		stateDir: stateDir,
	}, nil
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
				list = append(list, m.Summary())
			}
		}
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})

	return list, nil
}

// GetSnapshot reads a full manifest by SnapshotID
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
	return &m, nil
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
	var totalSourceBytes int64

	err := filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip unreadable paths
		}
		if info.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if e.shouldExclude(rel) {
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
		SnapshotID: snapID,
		CreatedAt:  startTime,
		NodeName:   e.cfg.NodeName,
		SourcePath: sourceDir,
		TotalFiles: len(fileList),
		TotalBytes: totalSourceBytes,
		Files:      make(map[string]FileEntry),
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

	// 3. Save local manifest
	manifestJSON, err := json.MarshalIndent(newManifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}

	localPath := filepath.Join(e.stateDir, snapID+".json")
	if err := os.WriteFile(localPath, manifestJSON, 0640); err != nil {
		return nil, fmt.Errorf("save local manifest: %w", err)
	}

	// 4. Encrypt and upload manifest to Telegram
	encManifest, err := newManifest.Encrypt(e.cfg.EncryptionKey)
	if err == nil {
		caption := fmt.Sprintf("[TCDRIVE_MANIFEST] %s | Files: %d | Size: %d", snapID, newManifest.TotalFiles, newManifest.TotalBytes)
		doc, _, err := e.tgClient.UploadDocument(ctx, snapID+".manifest.enc", encManifest, caption)
		if err == nil && doc != nil {
			newManifest.TelegramManifestID = doc.FileID
			// Update local manifest with Telegram manifest ID
			_ = os.WriteFile(localPath, manifestJSON, 0640)
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

	plan := CalculatePrunePlan(snapshots, e.cfg.Retention)
	for _, del := range plan.Delete {
		filePath := filepath.Join(e.stateDir, del.SnapshotID+".json")
		_ = os.Remove(filePath)
	}

	return plan, nil
}
