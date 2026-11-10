package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/crypto"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/snapshot"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/telegram"
)

// RestoreProgress reports live file restoration progress
type RestoreProgress struct {
	TotalFiles     int     `json:"total_files"`
	RestoredFiles  int     `json:"restored_files"`
	CurrentFile    string  `json:"current_file"`
	TotalBytes     int64   `json:"total_bytes"`
	RestoredBytes  int64   `json:"restored_bytes"`
	Percent        float64 `json:"percent"`
	IsComplete     bool    `json:"is_complete"`
	Error          string  `json:"error,omitempty"`
}

type RestoreCallback func(p RestoreProgress)

// Restorer coordinates point-in-time recovery
type Restorer struct {
	cfg      *config.Config
	engine   *snapshot.Engine
	tgClient *telegram.Client
}

// NewRestorer initializes the recovery service
func NewRestorer(cfg *config.Config, engine *snapshot.Engine, tgClient *telegram.Client) *Restorer {
	return &Restorer{
		cfg:      cfg,
		engine:   engine,
		tgClient: tgClient,
	}
}

// RestoreFile downloads, decrypts, and recovers a single file to destination
func (r *Restorer) RestoreFile(ctx context.Context, snapshotID, relPath, destPath string) error {
	m, err := r.engine.GetSnapshot(snapshotID)
	if err != nil {
		return fmt.Errorf("retrieve snapshot %s: %w", snapshotID, err)
	}

	cleanRel := filepath.Clean(filepath.ToSlash(relPath))
	fileEntry, exists := m.Files[cleanRel]
	if !exists {
		return fmt.Errorf("file %q not found in snapshot %s", relPath, snapshotID)
	}

	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create dest directory %s: %w", destDir, err)
	}

	tmpFile := destPath + ".tmp_restore"
	out, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create temp restore file: %w", err)
	}
	defer func() {
		out.Close()
		_ = os.Remove(tmpFile)
	}()

	for _, chunk := range fileEntry.Chunks {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		encData, err := r.tgClient.DownloadFile(ctx, chunk.TelegramFileID)
		if err != nil {
			return fmt.Errorf("download chunk %d: %w", chunk.Index, err)
		}

		plainData, err := crypto.DecryptChunk(encData, r.cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("decrypt chunk %d: %w", chunk.Index, err)
		}

		if chunk.Hash != "" {
			actualHash := crypto.HashData(plainData)
			if actualHash != chunk.Hash {
				return fmt.Errorf("chunk %d checksum mismatch: expected %s, got %s", chunk.Index, chunk.Hash, actualHash)
			}
		}

		if _, err := out.Write(plainData); err != nil {
			return fmt.Errorf("write chunk %d: %w", chunk.Index, err)
		}
	}

	out.Close()

	// Verify complete file hash if available
	if fileEntry.FileHash != "" {
		f, err := os.Open(tmpFile)
		if err == nil {
			hash, _, _ := crypto.HashStream(f)
			f.Close()
			if hash != fileEntry.FileHash {
				return errors.New("file overall SHA-256 integrity verification failed")
			}
		}
	}

	// Rename temp file to final destination
	if err := os.Rename(tmpFile, destPath); err != nil {
		return fmt.Errorf("finalize restored file: %w", err)
	}

	// Restore original modification timestamp
	if fileEntry.ModTime > 0 {
		mtime := time.Unix(fileEntry.ModTime, 0)
		_ = os.Chtimes(destPath, mtime, mtime)
	}

	return nil
}

// RestoreDirectory restores a specific subfolder or the entire snapshot
func (r *Restorer) RestoreDirectory(ctx context.Context, snapshotID, subDir, targetDir string, cb RestoreCallback) error {
	m, err := r.engine.GetSnapshot(snapshotID)
	if err != nil {
		return fmt.Errorf("retrieve snapshot %s: %w", snapshotID, err)
	}

	cleanSub := filepath.Clean(filepath.ToSlash(subDir))
	if cleanSub == "." || cleanSub == "/" {
		cleanSub = ""
	}

	var targets []snapshot.FileEntry
	var totalBytes int64

	for p, fe := range m.Files {
		if cleanSub == "" || strings.HasPrefix(p, cleanSub+"/") || p == cleanSub {
			targets = append(targets, fe)
			totalBytes += fe.Size
		}
	}

	if len(targets) == 0 {
		return fmt.Errorf("no files match path %q in snapshot %s", subDir, snapshotID)
	}

	prog := RestoreProgress{
		TotalFiles: len(targets),
		TotalBytes: totalBytes,
	}

	// Recreate all directory structures (including empty directories)
	for _, d := range m.Directories {
		cleanD := filepath.Clean(filepath.ToSlash(d))
		cleanD = strings.TrimPrefix(cleanD, "/")
		if cleanSub == "" || strings.HasPrefix(cleanD, cleanSub+"/") || cleanD == cleanSub {
			var relDest string
			if cleanSub == "" {
				relDest = cleanD
			} else {
				relDest = strings.TrimPrefix(cleanD, cleanSub)
				relDest = strings.TrimPrefix(relDest, "/")
			}
			if relDest != "" {
				fullDestDir := filepath.Join(targetDir, filepath.FromSlash(relDest))
				_ = os.MkdirAll(fullDestDir, 0755)
			}
		}
	}

	for idx, fe := range targets {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		var relDest string
		if cleanSub == "" {
			relDest = fe.Path
		} else if fe.Path == cleanSub {
			relDest = filepath.Base(fe.Path)
		} else {
			relDest = strings.TrimPrefix(fe.Path, cleanSub)
			relDest = strings.TrimPrefix(relDest, "/")
		}

		fullDestPath := filepath.Join(targetDir, filepath.FromSlash(relDest))

		prog.CurrentFile = fe.Path
		if cb != nil {
			cb(prog)
		}

		if err := r.RestoreFile(ctx, snapshotID, fe.Path, fullDestPath); err != nil {
			return fmt.Errorf("restore %s: %w", fe.Path, err)
		}

		prog.RestoredFiles = idx + 1
		prog.RestoredBytes += fe.Size
		if prog.TotalBytes > 0 {
			prog.Percent = float64(prog.RestoredBytes) / float64(prog.TotalBytes) * 100.0
		}
		if cb != nil {
			cb(prog)
		}
	}

	prog.IsComplete = true
	prog.Percent = 100.0
	if cb != nil {
		cb(prog)
	}

	return nil
}
