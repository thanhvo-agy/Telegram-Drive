package snapshot

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/crypto"
)

// ChunkRef holds metadata about an encrypted chunk stored on Telegram
type ChunkRef struct {
	Index          int    `json:"index"`
	Offset         int64  `json:"offset"`
	Size           int64  `json:"size"`
	EncryptedSize  int64  `json:"encrypted_size"`
	Hash           string `json:"hash"` // Plaintext SHA-256
	TelegramFileID string `json:"telegram_file_id"`
	TelegramMsgID  int64  `json:"telegram_msg_id"`
}

// FileEntry represents a file stored inside a snapshot
type FileEntry struct {
	Path     string     `json:"path"` // Relative path from SourcePath
	Size     int64      `json:"size"`
	ModTime  int64      `json:"mod_time"` // Unix seconds
	FileHash string     `json:"file_hash"`
	Chunks   []ChunkRef `json:"chunks"`
}

// SnapshotManifest is the complete point-in-time state of a backup
type SnapshotManifest struct {
	SnapshotID         string               `json:"snapshot_id"` // e.g. snap-20261110-143000
	CreatedAt          time.Time            `json:"created_at"`
	NodeName           string               `json:"node_name"`
	SourcePath         string               `json:"source_path"`
	TotalFiles         int                  `json:"total_files"`
	TotalBytes         int64                `json:"total_bytes"`
	UploadedBytes      int64                `json:"uploaded_bytes"`
	DeduplicatedBytes  int64                `json:"deduplicated_bytes"`
	DurationSec        float64              `json:"duration_sec"`
	TelegramManifestID string               `json:"telegram_manifest_id,omitempty"`
	Directories        []string             `json:"directories,omitempty"`
	Files              map[string]FileEntry `json:"files"`
	WORM               WORMMetadata         `json:"worm"`
	Tampered           bool                 `json:"tampered,omitempty"`
}

// SnapshotSummary is a lightweight metadata view for lists & UI timelines
type SnapshotSummary struct {
	SnapshotID        string    `json:"snapshot_id"`
	CreatedAt         time.Time `json:"created_at"`
	NodeName          string    `json:"node_name"`
	TotalFiles        int       `json:"total_files"`
	TotalBytes        int64     `json:"total_bytes"`
	UploadedBytes     int64     `json:"uploaded_bytes"`
	DeduplicatedBytes int64     `json:"deduplicated_bytes"`
	DurationSec       float64   `json:"duration_sec"`
	WORMEnabled       bool      `json:"worm_enabled"`
	WORMLocked        bool      `json:"worm_locked"`
	WORMMode          WORMMode  `json:"worm_mode,omitempty"`
	WORMRetainUntil   time.Time `json:"worm_retain_until,omitempty"`
	WORMLegalHold     bool      `json:"worm_legal_hold,omitempty"`
	Tampered          bool      `json:"tampered,omitempty"`
}

// Summary returns a lightweight summary of the manifest
func (m *SnapshotManifest) Summary() SnapshotSummary {
	now := time.Now()
	return SnapshotSummary{
		SnapshotID:        m.SnapshotID,
		CreatedAt:         m.CreatedAt,
		NodeName:          m.NodeName,
		TotalFiles:        m.TotalFiles,
		TotalBytes:        m.TotalBytes,
		UploadedBytes:     m.UploadedBytes,
		DeduplicatedBytes: m.DeduplicatedBytes,
		DurationSec:       m.DurationSec,
		WORMEnabled:       m.WORM.Enabled,
		WORMLocked:        m.IsWORMLocked(now),
		WORMMode:          m.WORM.Mode,
		WORMRetainUntil:   m.WORM.RetainUntil,
		WORMLegalHold:     m.WORM.LegalHold,
		Tampered:          m.Tampered,
	}
}

// EncryptSerializes manifest to JSON and encrypts it with AES-256-GCM
func (m *SnapshotManifest) Encrypt(passphrase string) ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	return crypto.EncryptChunk(data, passphrase)
}

// DecryptManifest decrypts an encrypted manifest payload
func DecryptManifest(data []byte, passphrase string) (*SnapshotManifest, error) {
	plaintext, err := crypto.DecryptChunk(data, passphrase)
	if err != nil {
		return nil, fmt.Errorf("decrypt manifest: %w", err)
	}

	var m SnapshotManifest
	if err := json.Unmarshal(plaintext, &m); err != nil {
		return nil, fmt.Errorf("unmarshal decrypted manifest: %w", err)
	}
	return &m, nil
}

// DiffStatus represents change state between two snapshots
type DiffStatus string

const (
	DiffAdded     DiffStatus = "ADDED"
	DiffModified  DiffStatus = "MODIFIED"
	DiffDeleted   DiffStatus = "DELETED"
	DiffUnchanged DiffStatus = "UNCHANGED"
)

// FileDiff holds diff details for a single file
type FileDiff struct {
	Path      string     `json:"path"`
	Status    DiffStatus `json:"status"`
	OldSize   int64      `json:"old_size,omitempty"`
	NewSize   int64      `json:"new_size,omitempty"`
	OldHash   string     `json:"old_hash,omitempty"`
	NewHash   string     `json:"new_hash,omitempty"`
	Timestamp int64      `json:"timestamp"`
}

// DiffManifests compares older snapshot (A) against newer snapshot (B)
func DiffManifests(a, b *SnapshotManifest) []FileDiff {
	var diffs []FileDiff

	seen := make(map[string]bool)

	// Check files in newer snapshot B
	for path, fileB := range b.Files {
		seen[path] = true
		if fileA, exists := a.Files[path]; exists {
			if fileA.FileHash == fileB.FileHash {
				diffs = append(diffs, FileDiff{
					Path:      path,
					Status:    DiffUnchanged,
					OldSize:   fileA.Size,
					NewSize:   fileB.Size,
					OldHash:   fileA.FileHash,
					NewHash:   fileB.FileHash,
					Timestamp: fileB.ModTime,
				})
			} else {
				diffs = append(diffs, FileDiff{
					Path:      path,
					Status:    DiffModified,
					OldSize:   fileA.Size,
					NewSize:   fileB.Size,
					OldHash:   fileA.FileHash,
					NewHash:   fileB.FileHash,
					Timestamp: fileB.ModTime,
				})
			}
		} else {
			diffs = append(diffs, FileDiff{
				Path:      path,
				Status:    DiffAdded,
				NewSize:   fileB.Size,
				NewHash:   fileB.FileHash,
				Timestamp: fileB.ModTime,
			})
		}
	}

	// Check deleted files (in A but not in B)
	for path, fileA := range a.Files {
		if !seen[path] {
			diffs = append(diffs, FileDiff{
				Path:      path,
				Status:    DiffDeleted,
				OldSize:   fileA.Size,
				OldHash:   fileA.FileHash,
				Timestamp: fileA.ModTime,
			})
		}
	}

	sort.Slice(diffs, func(i, j int) bool {
		return diffs[i].Path < diffs[j].Path
	})

	return diffs
}

// FileStationItem represents an item (file or folder) in the visual File Station explorer
type FileStationItem struct {
	Name         string    `json:"name"`
	Path         string    `json:"path"`
	IsDir        bool      `json:"is_dir"`
	Size         int64     `json:"size"`
	ModTime      time.Time `json:"mod_time"`
	Hash         string    `json:"hash,omitempty"`
	ChunkCount   int       `json:"chunk_count,omitempty"`
	Status       string    `json:"status"` // "synced", "modified", "deleted"
	VersionCount int       `json:"version_count"`
}

// ListDirectory returns directory listing for File Station view for a specific virtual folder
func (m *SnapshotManifest) ListDirectory(targetDir string) []FileStationItem {
	targetDir = filepath.Clean(filepath.ToSlash(targetDir))
	if targetDir == "." || targetDir == "/" {
		targetDir = ""
	}
	targetDir = strings.TrimPrefix(targetDir, "/")
	targetDir = strings.TrimSuffix(targetDir, "/")

	subDirs := make(map[string]time.Time)
	var items []FileStationItem

	// 1. Process files
	for p, fe := range m.Files {
		cleanP := filepath.Clean(filepath.ToSlash(p))
		cleanP = strings.TrimPrefix(cleanP, "/")
		var rel string
		if targetDir == "" {
			rel = cleanP
		} else if strings.HasPrefix(cleanP, targetDir+"/") {
			rel = strings.TrimPrefix(cleanP, targetDir+"/")
		} else {
			continue
		}

		parts := strings.Split(rel, "/")
		if len(parts) == 1 {
			// Direct file in this directory
			items = append(items, FileStationItem{
				Name:         parts[0],
				Path:         cleanP,
				IsDir:        false,
				Size:         fe.Size,
				ModTime:      time.Unix(fe.ModTime, 0),
				Hash:         fe.FileHash,
				ChunkCount:   len(fe.Chunks),
				Status:       "synced",
				VersionCount: 1,
			})
		} else {
			// Subdirectory
			dirName := parts[0]
			if _, exists := subDirs[dirName]; !exists {
				subDirs[dirName] = time.Unix(fe.ModTime, 0)
			}
		}
	}

	// 2. Process recorded directories (including empty subfolders)
	for _, d := range m.Directories {
		cleanD := filepath.Clean(filepath.ToSlash(d))
		cleanD = strings.TrimPrefix(cleanD, "/")
		var rel string
		if targetDir == "" {
			rel = cleanD
		} else if strings.HasPrefix(cleanD, targetDir+"/") {
			rel = strings.TrimPrefix(cleanD, targetDir+"/")
		} else {
			continue
		}

		parts := strings.Split(rel, "/")
		dirName := parts[0]
		if _, exists := subDirs[dirName]; !exists {
			subDirs[dirName] = m.CreatedAt
		}
	}

	// Add subdirectories
	for dirName, mtime := range subDirs {
		dirPath := dirName
		if targetDir != "" {
			dirPath = targetDir + "/" + dirName
		}
		items = append(items, FileStationItem{
			Name:         dirName,
			Path:         dirPath,
			IsDir:        true,
			ModTime:      mtime,
			Status:       "synced",
			VersionCount: 1,
		})
	}

	// Sort folders first, then files alphabetically
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})

	return items
}
