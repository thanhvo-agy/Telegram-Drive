package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
)

// TestEmpirical_CatastrophicRansomwareWipe_100PercentFidelity tests a simulated
// ransomware attack where the entire local snapshots directory is completely deleted,
// followed by disaster recovery reindex from Telegram.
// It verifies 100% exact fidelity of manifests, file entries, directory trees, and WORM statuses.
func TestEmpirical_CatastrophicRansomwareWipe_100PercentFidelity(t *testing.T) {
	key := "enterprise-recovery-master-key-32b!"
	baseTime := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

	// Step 1: Create a comprehensive suite of 5 diverse snapshots
	// Snapshot 1: Baseline snapshot with nested directory tree and multiple files/chunks
	s1 := &SnapshotManifest{
		SnapshotID:  "snap-20261110-010000",
		CreatedAt:   baseTime,
		NodeName:    "NAS-Enterprise-01",
		SourcePath:  "/volume1/data",
		TotalFiles:  4,
		TotalBytes:  31457280, // 30 MB
		DurationSec: 4.5,
		Directories: []string{
			"documents",
			"documents/finance",
			"documents/finance/2026",
			"photos",
			"photos/raw",
			"empty_folder",
		},
		Files: map[string]FileEntry{
			"documents/finance/2026/budget.xlsx": {
				Path:     "documents/finance/2026/budget.xlsx",
				Size:     10485760,
				ModTime:  baseTime.Add(-10 * time.Minute).Unix(),
				FileHash: "hash-budget-v1",
				Chunks: []ChunkRef{
					{
						Index:          0,
						Offset:         0,
						Size:           5242880,
						EncryptedSize:  5242908,
						Hash:           "hash-budget-c0",
						TelegramFileID: "tg-file-budget-c0",
						TelegramMsgID:  1001,
					},
					{
						Index:          1,
						Offset:         5242880,
						Size:           5242880,
						EncryptedSize:  5242908,
						Hash:           "hash-budget-c1",
						TelegramFileID: "tg-file-budget-c1",
						TelegramMsgID:  1002,
					},
				},
			},
			"documents/readme.txt": {
				Path:     "documents/readme.txt",
				Size:     2048,
				ModTime:  baseTime.Add(-2 * time.Hour).Unix(),
				FileHash: "hash-readme-v1",
				Chunks: []ChunkRef{
					{
						Index:          0,
						Offset:         0,
						Size:           2048,
						EncryptedSize:  2076,
						Hash:           "hash-readme-c0",
						TelegramFileID: "tg-file-readme-c0",
						TelegramMsgID:  1003,
					},
				},
			},
			"photos/raw/pic1.raw": {
				Path:     "photos/raw/pic1.raw",
				Size:     20971520,
				ModTime:  baseTime.Add(-1 * time.Hour).Unix(),
				FileHash: "hash-pic1-v1",
				Chunks: []ChunkRef{
					{
						Index:          0,
						Offset:         0,
						Size:           20971520,
						EncryptedSize:  20971548,
						Hash:           "hash-pic1-c0",
						TelegramFileID: "tg-file-pic1-c0",
						TelegramMsgID:  1004,
					},
				},
			},
		},
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      baseTime,
			RetainUntil:   baseTime.Add(60 * 24 * time.Hour), // 60 days active lock
			RetentionDays: 60,
			LegalHold:     false,
		},
	}
	s1.WORM.ManifestHMAC = s1.ComputeHMAC(key)

	// Snapshot 2: CoW deduplicated incremental snapshot
	s2Time := baseTime.Add(1 * time.Hour)
	s2 := &SnapshotManifest{
		SnapshotID:        "snap-20261110-020000",
		CreatedAt:         s2Time,
		NodeName:          "NAS-Enterprise-01",
		SourcePath:        "/volume1/data",
		TotalFiles:        4,
		TotalBytes:        32457280,
		UploadedBytes:     1000000,
		DeduplicatedBytes: 31457280,
		DurationSec:       1.2,
		Directories:       s1.Directories,
		Files: map[string]FileEntry{
			"documents/finance/2026/budget.xlsx": s1.Files["documents/finance/2026/budget.xlsx"], // CoW deduplicated
			"documents/readme.txt":               s1.Files["documents/readme.txt"],               // CoW deduplicated
			"photos/raw/pic1.raw":                s1.Files["photos/raw/pic1.raw"],                // CoW deduplicated
			"documents/finance/notes.txt": {
				Path:     "documents/finance/notes.txt",
				Size:     1000000,
				ModTime:  s2Time.Unix(),
				FileHash: "hash-notes-v1",
				Chunks: []ChunkRef{
					{
						Index:          0,
						Offset:         0,
						Size:           1000000,
						EncryptedSize:  1000028,
						Hash:           "hash-notes-c0",
						TelegramFileID: "tg-file-notes-c0",
						TelegramMsgID:  1005,
					},
				},
			},
		},
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      s2Time,
			RetainUntil:   s2Time.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
			LegalHold:     false,
		},
	}
	s2.WORM.ManifestHMAC = s2.ComputeHMAC(key)

	// Snapshot 3: Legal Hold active snapshot (RetainUntil is in the past, but LegalHold is true)
	s3Time := baseTime.Add(-100 * 24 * time.Hour)
	s3 := &SnapshotManifest{
		SnapshotID:  "snap-20260802-090000",
		CreatedAt:   s3Time,
		NodeName:    "NAS-Enterprise-01",
		SourcePath:  "/volume1/data",
		TotalFiles:  1,
		TotalBytes:  500,
		Directories: []string{"legal"},
		Files: map[string]FileEntry{
			"legal/audit.log": {
				Path:     "legal/audit.log",
				Size:     500,
				ModTime:  s3Time.Unix(),
				FileHash: "hash-audit-v1",
				Chunks: []ChunkRef{
					{
						Index:          0,
						Offset:         0,
						Size:           500,
						EncryptedSize:  528,
						Hash:           "hash-audit-c0",
						TelegramFileID: "tg-file-audit-c0",
						TelegramMsgID:  1006,
					},
				},
			},
		},
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      s3Time,
			RetainUntil:   s3Time.Add(30 * 24 * time.Hour), // Expired 70 days ago
			RetentionDays: 30,
			LegalHold:     true, // Active legal hold overrides expiration
		},
	}
	s3.WORM.ManifestHMAC = s3.ComputeHMAC(key)

	// Snapshot 4: Expired WORM snapshot (no legal hold, RetainUntil is in the past)
	s4Time := baseTime.Add(-40 * 24 * time.Hour)
	s4 := &SnapshotManifest{
		SnapshotID:  "snap-20261001-080000",
		CreatedAt:   s4Time,
		NodeName:    "NAS-Enterprise-01",
		SourcePath:  "/volume1/data",
		TotalFiles:  1,
		TotalBytes:  1024,
		Directories: []string{"temp"},
		Files: map[string]FileEntry{
			"temp/temp.txt": {
				Path:     "temp/temp.txt",
				Size:     1024,
				ModTime:  s4Time.Unix(),
				FileHash: "hash-temp-v1",
				Chunks: []ChunkRef{
					{
						Index:          0,
						Offset:         0,
						Size:           1024,
						EncryptedSize:  1052,
						Hash:           "hash-temp-c0",
						TelegramFileID: "tg-file-temp-c0",
						TelegramMsgID:  1007,
					},
				},
			},
		},
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      s4Time,
			RetainUntil:   s4Time.Add(10 * 24 * time.Hour), // Expired 30 days ago
			RetentionDays: 10,
			LegalHold:     false,
		},
	}
	s4.WORM.ManifestHMAC = s4.ComputeHMAC(key)

	// Snapshot 5: Governance mode snapshot
	s5Time := baseTime.Add(3 * time.Hour)
	s5 := &SnapshotManifest{
		SnapshotID:  "snap-20261110-040000",
		CreatedAt:   s5Time,
		NodeName:    "NAS-Enterprise-01",
		SourcePath:  "/volume1/data",
		TotalFiles:  2,
		TotalBytes:  8192,
		Directories: []string{"governance", "governance/reports"},
		Files: map[string]FileEntry{
			"governance/reports/gov.doc": {
				Path:     "governance/reports/gov.doc",
				Size:     8192,
				ModTime:  s5Time.Unix(),
				FileHash: "hash-gov-v1",
				Chunks: []ChunkRef{
					{
						Index:          0,
						Offset:         0,
						Size:           8192,
						EncryptedSize:  8220,
						Hash:           "hash-gov-c0",
						TelegramFileID: "tg-file-gov-c0",
						TelegramMsgID:  1008,
					},
				},
			},
		},
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeGovernance,
			LockedAt:      s5Time,
			RetainUntil:   s5Time.Add(14 * 24 * time.Hour),
			RetentionDays: 14,
			LegalHold:     false,
		},
	}
	s5.WORM.ManifestHMAC = s5.ComputeHMAC(key)

	originalManifests := []*SnapshotManifest{s1, s2, s3, s4, s5}

	// Encrypt all genuine manifests
	encManifestMap := make(map[string][]byte)
	for _, m := range originalManifests {
		enc, err := m.Encrypt(key)
		if err != nil {
			t.Fatalf("failed to encrypt manifest %s: %v", m.SnapshotID, err)
		}
		encManifestMap[m.SnapshotID] = enc
	}

	// Step 2: Build Mock Telegram Server with genuine manifests AND adversarial payloads:
	// - 1 regular chunk upload (non-manifest)
	// - 1 corrupted ciphertext manifest
	// - 1 tampered manifest (HMAC forged)
	tamperedManifest := &SnapshotManifest{
		SnapshotID: "snap-tampered-adversary",
		CreatedAt:  baseTime,
		NodeName:   "Evil-Node",
		TotalFiles: 1,
		TotalBytes: 100,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      baseTime,
			RetainUntil:   baseTime.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
			ManifestHMAC:  "bad-forged-hmac-signature-should-fail",
		},
	}
	tamperedEnc, _ := tamperedManifest.Encrypt(key)
	encManifestMap["snap-tampered-adversary"] = tamperedEnc

	mux := http.NewServeMux()
	mux.HandleFunc("/bottokenmesh/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		offsetStr := r.URL.Query().Get("offset")
		var offset int64
		if offsetStr != "" {
			fmt.Sscanf(offsetStr, "%d", &offset)
		}

		var rawUpdates []map[string]interface{}

		// 1. Genuine manifests
		for idx, m := range originalManifests {
			rawUpdates = append(rawUpdates, map[string]interface{}{
				"update_id": int64(10 + idx),
				"message": map[string]interface{}{
					"message_id": int64(2000 + idx),
					"caption":    fmt.Sprintf("[TCDRIVE_MANIFEST] %s | Files: %d | Size: %d", m.SnapshotID, m.TotalFiles, m.TotalBytes),
					"document": map[string]interface{}{
						"file_id":   "fid_" + m.SnapshotID,
						"file_name": m.SnapshotID + ".manifest.enc",
					},
				},
			})
		}

		// 2. Adversarial payload: non-manifest document
		rawUpdates = append(rawUpdates, map[string]interface{}{
			"update_id": int64(90),
			"message": map[string]interface{}{
				"message_id": int64(3001),
				"caption":    "[CHUNK] file.dat part 0",
				"document": map[string]interface{}{
					"file_id":   "fid_chunk_regular",
					"file_name": "file.dat.chunk.0000.enc",
				},
			},
		})

		// 3. Adversarial payload: corrupted ciphertext
		rawUpdates = append(rawUpdates, map[string]interface{}{
			"update_id": int64(91),
			"message": map[string]interface{}{
				"message_id": int64(3002),
				"caption":    "[TCDRIVE_MANIFEST] snap-corrupt-001 | Files: 1 | Size: 100",
				"document": map[string]interface{}{
					"file_id":   "fid_corrupted_data",
					"file_name": "snap-corrupt-001.manifest.enc",
				},
			},
		})

		// 4. Adversarial payload: tampered HMAC manifest
		rawUpdates = append(rawUpdates, map[string]interface{}{
			"update_id": int64(92),
			"message": map[string]interface{}{
				"message_id": int64(3003),
				"caption":    "[TCDRIVE_MANIFEST] snap-tampered-adversary | Files: 1 | Size: 100",
				"document": map[string]interface{}{
					"file_id":   "fid_snap-tampered-adversary",
					"file_name": "snap-tampered-adversary.manifest.enc",
				},
			},
		})

		var updates []map[string]interface{}
		for _, up := range rawUpdates {
			uid := up["update_id"].(int64)
			if uid >= offset {
				updates = append(updates, up)
			}
		}

		resp := map[string]interface{}{
			"ok":     true,
			"result": updates,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/bottokenmesh/getFile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fid := r.URL.Query().Get("file_id")
		resp := map[string]interface{}{
			"ok": true,
			"result": map[string]interface{}{
				"file_id":   fid,
				"file_path": "payloads/" + fid,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/file/bottokenmesh/payloads/", func(w http.ResponseWriter, r *http.Request) {
		fid := filepath.Base(r.URL.Path)
		if fid == "fid_corrupted_data" {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("not_encrypted_data_garbage_corrupted"))
			return
		}
		snapID := strings.TrimPrefix(fid, "fid_")
		if enc, exists := encManifestMap[snapID]; exists {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(enc)
			return
		}
		http.NotFound(w, r)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	t.Setenv("TELEGRAM_API_URL", server.URL)

	// Step 3: Populate local stateDir first to verify baseline
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "nas_snapshots")
	if err := os.MkdirAll(stateDir, 0750); err != nil {
		t.Fatalf("failed to create initial stateDir: %v", err)
	}

	for _, m := range originalManifests {
		data, _ := json.MarshalIndent(m, "", "  ")
		_ = os.WriteFile(filepath.Join(stateDir, m.SnapshotID+".json"), data, 0640)
	}

	cfg := &config.Config{
		BotTokens:     []string{"tokenmesh"},
		ChannelID:     "-100999888777",
		EncryptionKey: key,
		StateDir:      stateDir,
	}

	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("failed to init engine: %v", err)
	}

	preWipeList, err := engine.ListSnapshots()
	if err != nil || len(preWipeList) != 5 {
		t.Fatalf("pre-wipe snapshot count mismatch: expected 5, got %d (err: %v)", len(preWipeList), err)
	}

	// Step 4: CATASTROPHIC RANSOMWARE ATTACK SIMULATION
	// Completely format / delete the stateDir on disk!
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatalf("failed to execute catastrophic filesystem wipe: %v", err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("state directory still exists after simulated format!")
	}

	// Verify local repository is completely wiped
	postWipeList, _ := engine.ListSnapshots()
	if len(postWipeList) != 0 {
		t.Fatalf("snapshots still visible after catastrophic wipe: %d", len(postWipeList))
	}

	// Step 5: DISASTER RECOVERY EXECUTION
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	summary, err := engine.ReindexFromTelegram(ctx)
	if err != nil {
		t.Fatalf("ReindexFromTelegram failed with error: %v", err)
	}

	// Verify reindex summary counts
	if summary.ManifestsRestored != 5 {
		t.Fatalf("expected 5 manifests restored, got %d (summary: %+v)", summary.ManifestsRestored, summary)
	}
	if summary.ManifestsFound != 7 { // 5 genuine + 1 corrupted + 1 tampered
		t.Errorf("expected 7 manifest captions found, got %d", summary.ManifestsFound)
	}
	if summary.ManifestsSkipped != 2 { // 1 corrupted + 1 tampered skipped
		t.Errorf("expected 2 manifests skipped, got %d", summary.ManifestsSkipped)
	}

	// Step 6: 100% FIDELITY VERIFICATION
	// Re-list restored snapshots
	restoredList, err := engine.ListSnapshots()
	if err != nil {
		t.Fatalf("failed to list snapshots after reindex: %v", err)
	}
	if len(restoredList) != 5 {
		t.Fatalf("expected 5 restored snapshots in engine, got %d", len(restoredList))
	}

	now := time.Now()

	for _, orig := range originalManifests {
		t.Run("Fidelity_"+orig.SnapshotID, func(t *testing.T) {
			restored, err := engine.GetSnapshot(orig.SnapshotID)
			if err != nil {
				t.Fatalf("failed to retrieve restored snapshot %s: %v", orig.SnapshotID, err)
			}

			// A. Core Metadata Fidelity
			if restored.SnapshotID != orig.SnapshotID {
				t.Errorf("SnapshotID mismatch: orig=%s, restored=%s", orig.SnapshotID, restored.SnapshotID)
			}
			if restored.CreatedAt.Unix() != orig.CreatedAt.Unix() {
				t.Errorf("CreatedAt mismatch: orig=%d, restored=%d", orig.CreatedAt.Unix(), restored.CreatedAt.Unix())
			}
			if restored.NodeName != orig.NodeName {
				t.Errorf("NodeName mismatch: orig=%s, restored=%s", orig.NodeName, restored.NodeName)
			}
			if restored.SourcePath != orig.SourcePath {
				t.Errorf("SourcePath mismatch: orig=%s, restored=%s", orig.SourcePath, restored.SourcePath)
			}
			if restored.TotalFiles != orig.TotalFiles {
				t.Errorf("TotalFiles mismatch: orig=%d, restored=%d", orig.TotalFiles, restored.TotalFiles)
			}
			if restored.TotalBytes != orig.TotalBytes {
				t.Errorf("TotalBytes mismatch: orig=%d, restored=%d", orig.TotalBytes, restored.TotalBytes)
			}
			if restored.UploadedBytes != orig.UploadedBytes {
				t.Errorf("UploadedBytes mismatch: orig=%d, restored=%d", orig.UploadedBytes, restored.UploadedBytes)
			}
			if restored.DeduplicatedBytes != orig.DeduplicatedBytes {
				t.Errorf("DeduplicatedBytes mismatch: orig=%d, restored=%d", orig.DeduplicatedBytes, restored.DeduplicatedBytes)
			}

			// B. Directory Tree Fidelity
			if !reflect.DeepEqual(restored.Directories, orig.Directories) {
				t.Errorf("Directory tree mismatch:\norig:     %+v\nrestored: %+v", orig.Directories, restored.Directories)
			}
			// Test Directory Listings
			origRoot := orig.ListDirectory("")
			restoredRoot := restored.ListDirectory("")
			if len(origRoot) != len(restoredRoot) {
				t.Errorf("Root directory item count mismatch: orig=%d, restored=%d", len(origRoot), len(restoredRoot))
			}
			for i := range origRoot {
				if origRoot[i].Name != restoredRoot[i].Name || origRoot[i].IsDir != restoredRoot[i].IsDir {
					t.Errorf("Root directory item %d mismatch: orig=%+v, restored=%+v", i, origRoot[i], restoredRoot[i])
				}
			}

			// C. File Entries & ChunkRefs Fidelity
			if len(restored.Files) != len(orig.Files) {
				t.Fatalf("File count mismatch: orig=%d, restored=%d", len(orig.Files), len(restored.Files))
			}
			for filePath, origEntry := range orig.Files {
				restoredEntry, exists := restored.Files[filePath]
				if !exists {
					t.Fatalf("File %s missing in restored manifest", filePath)
				}
				if restoredEntry.Path != origEntry.Path {
					t.Errorf("FilePath mismatch: orig=%s, restored=%s", origEntry.Path, restoredEntry.Path)
				}
				if restoredEntry.Size != origEntry.Size {
					t.Errorf("FileSize mismatch for %s: orig=%d, restored=%d", filePath, origEntry.Size, restoredEntry.Size)
				}
				if restoredEntry.ModTime != origEntry.ModTime {
					t.Errorf("ModTime mismatch for %s: orig=%d, restored=%d", filePath, origEntry.ModTime, restoredEntry.ModTime)
				}
				if restoredEntry.FileHash != origEntry.FileHash {
					t.Errorf("FileHash mismatch for %s: orig=%s, restored=%s", filePath, origEntry.FileHash, restoredEntry.FileHash)
				}
				if len(restoredEntry.Chunks) != len(origEntry.Chunks) {
					t.Fatalf("Chunk count mismatch for %s: orig=%d, restored=%d", filePath, len(origEntry.Chunks), len(restoredEntry.Chunks))
				}
				for cIdx := range origEntry.Chunks {
					oc := origEntry.Chunks[cIdx]
					rc := restoredEntry.Chunks[cIdx]
					if oc.Index != rc.Index || oc.Offset != rc.Offset || oc.Size != rc.Size ||
						oc.EncryptedSize != rc.EncryptedSize || oc.Hash != rc.Hash ||
						oc.TelegramFileID != rc.TelegramFileID || oc.TelegramMsgID != rc.TelegramMsgID {
						t.Errorf("Chunk %d mismatch for %s:\norig:     %+v\nrestored: %+v", cIdx, filePath, oc, rc)
					}
				}
			}

			// D. WORM Metadata & Status Fidelity
			if restored.WORM.Enabled != orig.WORM.Enabled {
				t.Errorf("WORM.Enabled mismatch: orig=%v, restored=%v", orig.WORM.Enabled, restored.WORM.Enabled)
			}
			if restored.WORM.Mode != orig.WORM.Mode {
				t.Errorf("WORM.Mode mismatch: orig=%s, restored=%s", orig.WORM.Mode, restored.WORM.Mode)
			}
			if restored.WORM.LockedAt.Unix() != orig.WORM.LockedAt.Unix() {
				t.Errorf("WORM.LockedAt mismatch: orig=%d, restored=%d", orig.WORM.LockedAt.Unix(), restored.WORM.LockedAt.Unix())
			}
			if restored.WORM.RetainUntil.Unix() != orig.WORM.RetainUntil.Unix() {
				t.Errorf("WORM.RetainUntil mismatch: orig=%d, restored=%d", orig.WORM.RetainUntil.Unix(), restored.WORM.RetainUntil.Unix())
			}
			if restored.WORM.RetentionDays != orig.WORM.RetentionDays {
				t.Errorf("WORM.RetentionDays mismatch: orig=%d, restored=%d", orig.WORM.RetentionDays, restored.WORM.RetentionDays)
			}
			if restored.WORM.LegalHold != orig.WORM.LegalHold {
				t.Errorf("WORM.LegalHold mismatch: orig=%v, restored=%v", orig.WORM.LegalHold, restored.WORM.LegalHold)
			}
			if restored.WORM.ManifestHMAC != orig.WORM.ManifestHMAC {
				t.Errorf("WORM.ManifestHMAC mismatch: orig=%s, restored=%s", orig.WORM.ManifestHMAC, restored.WORM.ManifestHMAC)
			}

			// E. Cryptographic Integrity & Tamper Flag
			if restored.Tampered {
				t.Errorf("restored snapshot %s erroneously flagged as tampered", orig.SnapshotID)
			}
			if !restored.VerifyHMAC(key) {
				t.Errorf("restored snapshot %s failed HMAC verification with master key", orig.SnapshotID)
			}

			// F. IsWORMLocked Fidelity
			expectedLocked := orig.IsWORMLocked(now)
			actualLocked := restored.IsWORMLocked(now)
			if actualLocked != expectedLocked {
				t.Errorf("IsWORMLocked mismatch for %s: orig=%v, restored=%v", orig.SnapshotID, expectedLocked, actualLocked)
			}
		})
	}
}

// TestEmpirical_TelegramPagination_Over100Messages tests that ReindexFromTelegram
// correctly pages through Telegram updates when there are more than 100 messages (e.g. 250 messages)
// spread across multiple getUpdates pages using offset.
func TestEmpirical_TelegramPagination_Over100Messages(t *testing.T) {
	key := "pagination-test-master-key-32b!!"
	now := time.Now().Truncate(time.Second)

	// Create 3 genuine manifests placed on page 1, page 2, and page 3
	mPage1 := &SnapshotManifest{
		SnapshotID: "snap-page-01",
		CreatedAt:  now.Add(-3 * time.Hour),
		NodeName:   "Paging-Node",
		TotalFiles: 1,
		TotalBytes: 100,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      now.Add(-3 * time.Hour),
			RetainUntil:   now.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
		},
	}
	mPage1.WORM.ManifestHMAC = mPage1.ComputeHMAC(key)

	mPage2 := &SnapshotManifest{
		SnapshotID: "snap-page-02",
		CreatedAt:  now.Add(-2 * time.Hour),
		NodeName:   "Paging-Node",
		TotalFiles: 2,
		TotalBytes: 200,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      now.Add(-2 * time.Hour),
			RetainUntil:   now.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
		},
	}
	mPage2.WORM.ManifestHMAC = mPage2.ComputeHMAC(key)

	mPage3 := &SnapshotManifest{
		SnapshotID: "snap-page-03",
		CreatedAt:  now.Add(-1 * time.Hour),
		NodeName:   "Paging-Node",
		TotalFiles: 3,
		TotalBytes: 300,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      now.Add(-1 * time.Hour),
			RetainUntil:   now.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
		},
	}
	mPage3.WORM.ManifestHMAC = mPage3.ComputeHMAC(key)

	encMap := make(map[string][]byte)
	for _, m := range []*SnapshotManifest{mPage1, mPage2, mPage3} {
		enc, err := m.Encrypt(key)
		if err != nil {
			t.Fatalf("encrypt failed: %v", err)
		}
		encMap[m.SnapshotID] = enc
	}

	// Generate 250 simulated Telegram updates:
	// Update 42: snap-page-01
	// Update 142: snap-page-02
	// Update 242: snap-page-03
	// All other updates are normal chat messages or chunk uploads
	totalMessages := 250
	allUpdates := make([]map[string]interface{}, totalMessages)
	for i := 0; i < totalMessages; i++ {
		upID := int64(1000 + i)
		caption := fmt.Sprintf("[CHUNK] data part %d", i)
		fileID := fmt.Sprintf("fid_chunk_%d", i)

		if i == 42 {
			caption = fmt.Sprintf("[TCDRIVE_MANIFEST] %s | Files: 1 | Size: 100", mPage1.SnapshotID)
			fileID = "fid_" + mPage1.SnapshotID
		} else if i == 142 {
			caption = fmt.Sprintf("[TCDRIVE_MANIFEST] %s | Files: 2 | Size: 200", mPage2.SnapshotID)
			fileID = "fid_" + mPage2.SnapshotID
		} else if i == 242 {
			caption = fmt.Sprintf("[TCDRIVE_MANIFEST] %s | Files: 3 | Size: 300", mPage3.SnapshotID)
			fileID = "fid_" + mPage3.SnapshotID
		}

		allUpdates[i] = map[string]interface{}{
			"update_id": upID,
			"message": map[string]interface{}{
				"message_id": int64(5000 + i),
				"caption":    caption,
				"document": map[string]interface{}{
					"file_id":   fileID,
					"file_name": fileID + ".enc",
				},
			},
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/botpagetoken/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		offsetStr := r.URL.Query().Get("offset")
		var offset int64
		if offsetStr != "" {
			fmt.Sscanf(offsetStr, "%d", &offset)
		}

		// Filter updates where update_id >= offset, limit 100
		var pageResults []map[string]interface{}
		for _, up := range allUpdates {
			uid := up["update_id"].(int64)
			if uid >= offset {
				pageResults = append(pageResults, up)
				if len(pageResults) >= 100 {
					break
				}
			}
		}

		resp := map[string]interface{}{
			"ok":     true,
			"result": pageResults,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/botpagetoken/getFile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fid := r.URL.Query().Get("file_id")
		resp := map[string]interface{}{
			"ok": true,
			"result": map[string]interface{}{
				"file_id":   fid,
				"file_path": "files/" + fid,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/file/botpagetoken/files/", func(w http.ResponseWriter, r *http.Request) {
		fid := filepath.Base(r.URL.Path)
		snapID := strings.TrimPrefix(fid, "fid_")
		if enc, exists := encMap[snapID]; exists {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(enc)
			return
		}
		http.NotFound(w, r)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	t.Setenv("TELEGRAM_API_URL", server.URL)

	emptySnapDir := filepath.Join(t.TempDir(), "paged_snapshots")
	cfg := &config.Config{
		BotTokens:     []string{"pagetoken"},
		ChannelID:     "-100555444333",
		EncryptionKey: key,
		StateDir:      emptySnapDir,
	}

	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("failed to init engine: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	summary, err := engine.ReindexFromTelegram(ctx)
	if err != nil {
		t.Fatalf("ReindexFromTelegram with pagination failed: %v", err)
	}

	if summary.ScannedMessages != 250 {
		t.Errorf("expected 250 messages scanned across pages, got %d", summary.ScannedMessages)
	}
	if summary.ManifestsRestored != 3 {
		t.Errorf("expected 3 manifests restored across 3 pages, got %d", summary.ManifestsRestored)
	}

	// Verify all 3 manifests are present in stateDir
	for _, id := range []string{"snap-page-01", "snap-page-02", "snap-page-03"} {
		m, err := engine.GetSnapshot(id)
		if err != nil {
			t.Errorf("snapshot %s from page was not restored: %v", id, err)
		} else if m.Tampered {
			t.Errorf("snapshot %s was flagged tampered", id)
		}
	}
}

// TestEmpirical_ConcurrencyAndRaceStress tests Engine operations under heavy concurrent access
// with Go's race detector (-race).
func TestEmpirical_ConcurrencyAndRaceStress(t *testing.T) {
	tempDir := t.TempDir()
	snapDir := filepath.Join(tempDir, "race_snapshots")
	_ = os.MkdirAll(snapDir, 0755)

	key := "race-stress-test-master-key-32b!"
	now := time.Now().Truncate(time.Second)

	// Create 10 genuine snapshots
	for i := 0; i < 10; i++ {
		m := &SnapshotManifest{
			SnapshotID: fmt.Sprintf("snap-race-%02d", i),
			CreatedAt:  now.Add(-time.Duration(i) * time.Hour),
			NodeName:   "RaceNode",
			TotalFiles: i + 1,
			TotalBytes: int64((i + 1) * 1024),
			WORM: WORMMetadata{
				Enabled:       true,
				Mode:          WORMModeCompliance,
				LockedAt:      now.Add(-time.Duration(i) * time.Hour),
				RetainUntil:   now.Add(30 * 24 * time.Hour),
				RetentionDays: 30,
			},
			Directories: []string{"dir1", "dir1/subdir"},
			Files: map[string]FileEntry{
				"dir1/file.txt": {
					Path:     "dir1/file.txt",
					Size:     1024,
					ModTime:  now.Unix(),
					FileHash: fmt.Sprintf("hash-%d", i),
				},
			},
		}
		m.WORM.ManifestHMAC = m.ComputeHMAC(key)
		data, _ := json.MarshalIndent(m, "", "  ")
		_ = os.WriteFile(filepath.Join(snapDir, m.SnapshotID+".json"), data, 0644)
	}

	cfg := &config.Config{
		EncryptionKey: key,
		StateDir:      snapDir,
		Retention: config.RetentionConfig{
			KeepHourly: 5,
			KeepDaily:  5,
		},
		WORM: config.WORMConfig{
			Enabled:       true,
			Mode:          "compliance",
			RetentionDays: 30,
		},
	}

	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("failed to init engine: %v", err)
	}

	// Concurrently invoke ListSnapshots, GetSnapshot, LoadSnapshot, GetFileHistory,
	// CalculatePrunePlan, PruneSnapshots, StateDir, and SetStateDir.
	var wg sync.WaitGroup
	workers := 20
	iterations := 25

	// Group 1: Concurrent ListSnapshots
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				_, _ = engine.ListSnapshots()
			}
		}()
	}

	// Group 2: Concurrent GetSnapshot & LoadSnapshot
	for w := 0; w < workers; w++ {
		wg.Add(1)
		id := fmt.Sprintf("snap-race-%02d", w%10)
		go func(snapID string) {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				_, _ = engine.GetSnapshot(snapID)
				_, _ = engine.LoadSnapshot(snapID)
			}
		}(id)
	}

	// Group 3: Concurrent GetFileHistory
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				_, _ = engine.GetFileHistory("dir1/file.txt")
			}
		}()
	}

	// Group 4: Concurrent PruneSnapshots (should guard WORM snapshots and be safe)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := 0; it < 5; it++ {
				_, _ = engine.PruneSnapshots()
			}
		}()
	}

	// Group 5: Concurrent StateDir access
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				_ = engine.StateDir()
			}
		}()
	}

	wg.Wait()
}

// TestEmpirical_SetStateDir_DataRace verifies whether concurrent SetStateDir and DeleteSnapshot / PruneSnapshots
// triggers a data race on e.stateDir.
func TestEmpirical_SetStateDir_DataRace(t *testing.T) {
	tempDir := t.TempDir()
	snapDir1 := filepath.Join(tempDir, "snap1")
	snapDir2 := filepath.Join(tempDir, "snap2")
	_ = os.MkdirAll(snapDir1, 0755)
	_ = os.MkdirAll(snapDir2, 0755)

	cfg := &config.Config{
		StateDir: snapDir1,
	}
	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("failed to init engine: %v", err)
	}

	var wgWorkers sync.WaitGroup
	var wgMutator sync.WaitGroup
	stop := make(chan struct{})

	// Goroutine 1: Rapidly mutate StateDir
	wgMutator.Add(1)
	go func() {
		defer wgMutator.Done()
		flip := false
		for {
			select {
			case <-stop:
				return
			default:
				if flip {
					engine.SetStateDir(snapDir1)
				} else {
					engine.SetStateDir(snapDir2)
				}
				flip = !flip
			}
		}
	}()

	// Goroutine 2: Concurrently call DeleteSnapshot on existing snapshots
	wgWorkers.Add(1)
	go func() {
		defer wgWorkers.Done()
		for i := 0; i < 50; i++ {
			// Write unlocked file to current stateDir
			sid := fmt.Sprintf("snap-del-%d", i)
			m := &SnapshotManifest{
				SnapshotID: sid,
				CreatedAt:  time.Now(),
			}
			data, _ := json.Marshal(m)
			_ = os.WriteFile(filepath.Join(snapDir1, sid+".json"), data, 0644)
			_ = os.WriteFile(filepath.Join(snapDir2, sid+".json"), data, 0644)
			_ = engine.DeleteSnapshot(sid)
		}
	}()

	// Goroutine 3: Concurrently call PruneSnapshots
	wgWorkers.Add(1)
	go func() {
		defer wgWorkers.Done()
		for i := 0; i < 50; i++ {
			_, _ = engine.PruneSnapshots()
		}
	}()

	wgWorkers.Wait()
	close(stop)
	wgMutator.Wait()
}



