package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
)

// TestChallengerR2_WORMBypass_EnabledFalse_Matrix rigorously stress-tests
// all variations of the "enabled: false" bypass and retention tampering attack vectors.
func TestChallengerR2_WORMBypass_EnabledFalse_Matrix(t *testing.T) {
	tempDir := t.TempDir()
	snapDir := filepath.Join(tempDir, "snapshots")
	_ = os.MkdirAll(snapDir, 0755)

	key := "challenger-r2-super-secret-key-32b"
	now := time.Now().Truncate(time.Second)

	cfg := &config.Config{
		EncryptionKey: key,
		StateDir:      snapDir,
		Retention: config.RetentionConfig{
			KeepHourly: 1,
			KeepDaily:  1,
		},
		WORM: config.WORMConfig{
			Enabled:       true,
			Mode:          "compliance",
			RetentionDays: 30,
		},
	}

	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("Engine init failed: %v", err)
	}

	// Helper to create and save a genuine WORM snapshot
	createGenuineSnap := func(id string, retainUntil time.Time) *SnapshotManifest {
		snap := &SnapshotManifest{
			SnapshotID: id,
			CreatedAt:  now.Add(-1 * time.Hour),
			NodeName:   "Production-NAS-01",
			TotalFiles: 42,
			TotalBytes: 10485760,
			WORM: WORMMetadata{
				Enabled:       true,
				Mode:          WORMModeCompliance,
				LockedAt:      now.Add(-1 * time.Hour),
				RetainUntil:   retainUntil,
				RetentionDays: 30,
			},
			Files: map[string]FileEntry{
				"test.txt": {Path: "test.txt", Size: 100},
			},
		}
		snap.WORM.ManifestHMAC = snap.ComputeHMAC(key)
		data, _ := json.MarshalIndent(snap, "", "  ")
		snapPath := filepath.Join(snapDir, id+".json")
		if err := os.WriteFile(snapPath, data, 0644); err != nil {
			t.Fatalf("Write snap failed: %v", err)
		}
		return snap
	}

	// Subtest 1: Malware flips enabled: true -> enabled: false, leaves original HMAC
	t.Run("Flipping_Enabled_False_With_Original_HMAC", func(t *testing.T) {
		snapID := "snap-attack-001"
		createGenuineSnap(snapID, now.Add(30*24*time.Hour))
		snapPath := filepath.Join(snapDir, snapID+".json")

		raw, _ := os.ReadFile(snapPath)
		tampered := strings.Replace(string(raw), `"enabled": true`, `"enabled": false`, 1)
		_ = os.WriteFile(snapPath, []byte(tampered), 0644)

		loaded, err := engine.GetSnapshot(snapID)
		if err != nil {
			t.Fatalf("GetSnapshot failed: %v", err)
		}
		if !loaded.Tampered {
			t.Fatalf("EXPLOIT: Tampered was false when enabled flipped to false!")
		}

		// PruneSnapshots must abort immediately
		_, pruneErr := engine.PruneSnapshots()
		if pruneErr == nil {
			t.Fatalf("EXPLOIT: PruneSnapshots did not abort with tampered snapshot present!")
		}
		if !strings.Contains(pruneErr.Error(), "tampered snapshot detected") {
			t.Fatalf("Expected tampered snapshot prune error, got: %v", pruneErr)
		}

		// DeleteSnapshot must reject
		delErr := engine.DeleteSnapshot(snapID)
		if delErr == nil {
			t.Fatalf("EXPLOIT: DeleteSnapshot succeeded on tampered snapshot!")
		}
		if !errors.Is(delErr, ErrSnapshotWORMLocked) {
			t.Fatalf("Expected ErrSnapshotWORMLocked, got %v", delErr)
		}

		// File must still exist on disk
		if _, statErr := os.Stat(snapPath); statErr != nil {
			t.Fatalf("EXPLOIT: File was deleted from disk: %v", statErr)
		}

		_ = os.Remove(snapPath)
	})

	// Subtest 2: Malware flips enabled: false AND shortens retain_until into past
	t.Run("Flipping_Enabled_False_And_Shortening_RetainUntil", func(t *testing.T) {
		snapID := "snap-attack-002"
		orig := createGenuineSnap(snapID, now.Add(30*24*time.Hour))
		snapPath := filepath.Join(snapDir, snapID+".json")

		raw, _ := os.ReadFile(snapPath)
		tampered := strings.Replace(string(raw), `"enabled": true`, `"enabled": false`, 1)
		pastDate := "2019-01-01T00:00:00Z"
		tampered = strings.Replace(tampered, orig.WORM.RetainUntil.Format(time.RFC3339Nano), pastDate, 1)
		_ = os.WriteFile(snapPath, []byte(tampered), 0644)

		loaded, err := engine.GetSnapshot(snapID)
		if err != nil {
			t.Fatalf("GetSnapshot failed: %v", err)
		}
		if !loaded.Tampered {
			t.Fatalf("EXPLOIT: Tampered was false when enabled: false and retain_until manipulated!")
		}

		delErr := engine.DeleteSnapshot(snapID)
		if delErr == nil || !errors.Is(delErr, ErrSnapshotWORMLocked) {
			t.Fatalf("EXPLOIT: DeleteSnapshot did not reject tampered snapshot: %v", delErr)
		}

		if _, statErr := os.Stat(snapPath); statErr != nil {
			t.Fatalf("EXPLOIT: File was deleted: %v", statErr)
		}

		_ = os.Remove(snapPath)
	})

	// Subtest 3: Malware flips enabled: false AND removes manifest_hmac completely
	t.Run("Flipping_Enabled_False_And_Clearing_HMAC", func(t *testing.T) {
		snapID := "snap-attack-003"
		orig := createGenuineSnap(snapID, now.Add(30*24*time.Hour))
		snapPath := filepath.Join(snapDir, snapID+".json")

		raw, _ := os.ReadFile(snapPath)
		tampered := strings.Replace(string(raw), `"enabled": true`, `"enabled": false`, 1)
		tampered = strings.Replace(tampered, orig.WORM.ManifestHMAC, "", 1)
		_ = os.WriteFile(snapPath, []byte(tampered), 0644)

		// Because e.cfg.WORM.Enabled is true, verifyManifestIntegrity flags missing HMAC as Tampered
		loaded, err := engine.GetSnapshot(snapID)
		if err != nil {
			t.Fatalf("GetSnapshot failed: %v", err)
		}
		if !loaded.Tampered {
			t.Fatalf("EXPLOIT: Tampered was false when enabled: false and HMAC cleared on WORM-enabled node!")
		}

		delErr := engine.DeleteSnapshot(snapID)
		if delErr == nil || !errors.Is(delErr, ErrSnapshotWORMLocked) {
			t.Fatalf("EXPLOIT: DeleteSnapshot did not reject stripped HMAC: %v", delErr)
		}

		if _, statErr := os.Stat(snapPath); statErr != nil {
			t.Fatalf("EXPLOIT: File was deleted: %v", statErr)
		}

		_ = os.Remove(snapPath)
	})

	// Subtest 4: Malware changes retention_days from 30 to 0
	t.Run("Manipulating_RetentionDays_To_Zero", func(t *testing.T) {
		snapID := "snap-attack-004"
		createGenuineSnap(snapID, now.Add(30*24*time.Hour))
		snapPath := filepath.Join(snapDir, snapID+".json")

		raw, _ := os.ReadFile(snapPath)
		tampered := strings.Replace(string(raw), `"retention_days": 30`, `"retention_days": 0`, 1)
		_ = os.WriteFile(snapPath, []byte(tampered), 0644)

		loaded, err := engine.GetSnapshot(snapID)
		if err != nil {
			t.Fatalf("GetSnapshot failed: %v", err)
		}
		if !loaded.Tampered {
			t.Fatalf("EXPLOIT: Tampered was false when retention_days changed to 0!")
		}

		delErr := engine.DeleteSnapshot(snapID)
		if delErr == nil || !errors.Is(delErr, ErrSnapshotWORMLocked) {
			t.Fatalf("EXPLOIT: DeleteSnapshot did not reject retention_days tampering: %v", delErr)
		}

		_ = os.Remove(snapPath)
	})

	// Subtest 5: Malware recomputes HMAC with an unauthorized key
	t.Run("Forging_HMAC_With_Unauthorized_Key", func(t *testing.T) {
		snapID := "snap-attack-005"
		snap := createGenuineSnap(snapID, now.Add(30*24*time.Hour))
		snapPath := filepath.Join(snapDir, snapID+".json")

		snap.WORM.Enabled = false
		snap.WORM.ManifestHMAC = snap.ComputeHMAC("attacker-fake-key-999999999999")
		data, _ := json.MarshalIndent(snap, "", "  ")
		_ = os.WriteFile(snapPath, data, 0644)

		loaded, err := engine.GetSnapshot(snapID)
		if err != nil {
			t.Fatalf("GetSnapshot failed: %v", err)
		}
		if !loaded.Tampered {
			t.Fatalf("EXPLOIT: Tampered was false when HMAC forged with attacker key!")
		}

		delErr := engine.DeleteSnapshot(snapID)
		if delErr == nil || !errors.Is(delErr, ErrSnapshotWORMLocked) {
			t.Fatalf("EXPLOIT: DeleteSnapshot allowed forged HMAC: %v", delErr)
		}

		_ = os.Remove(snapPath)
	})
}

// TestChallengerR2_DeleteSnapshot_ExhaustiveTamperRejection verifies that
// DeleteSnapshot rejects manifests tampered in ANY canonical or cryptographic field.
func TestChallengerR2_DeleteSnapshot_ExhaustiveTamperRejection(t *testing.T) {
	tempDir := t.TempDir()
	snapDir := filepath.Join(tempDir, "snapshots")
	_ = os.MkdirAll(snapDir, 0755)

	key := "exhaustive-tamper-check-key-32b!"
	now := time.Now().Truncate(time.Second)

	cfg := &config.Config{
		EncryptionKey: key,
		StateDir:      snapDir,
		WORM: config.WORMConfig{
			Enabled:       true,
			Mode:          "compliance",
			RetentionDays: 30,
		},
	}

	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}

	makeBaseManifest := func(id string) *SnapshotManifest {
		m := &SnapshotManifest{
			SnapshotID: id,
			CreatedAt:  now.Add(-10 * time.Minute),
			NodeName:   "PrimaryNode",
			TotalFiles: 100,
			TotalBytes: 5000000,
			WORM: WORMMetadata{
				Enabled:       true,
				Mode:          WORMModeCompliance,
				LockedAt:      now.Add(-10 * time.Minute),
				RetainUntil:   now.Add(30 * 24 * time.Hour),
				RetentionDays: 30,
				LegalHold:     false,
			},
		}
		m.WORM.ManifestHMAC = m.ComputeHMAC(key)
		return m
	}

	tamperCases := []struct {
		name     string
		mutateFn func(m *SnapshotManifest)
	}{
		{"Mutate_SnapshotID", func(m *SnapshotManifest) { m.SnapshotID = "snap-tampered-id" }},
		{"Mutate_CreatedAt", func(m *SnapshotManifest) { m.CreatedAt = m.CreatedAt.Add(-24 * time.Hour) }},
		{"Mutate_NodeName", func(m *SnapshotManifest) { m.NodeName = "EvilNode" }},
		{"Mutate_TotalFiles", func(m *SnapshotManifest) { m.TotalFiles = 9999 }},
		{"Mutate_TotalBytes", func(m *SnapshotManifest) { m.TotalBytes = 1 }},
		{"Mutate_LockedAt", func(m *SnapshotManifest) { m.WORM.LockedAt = now.Add(-50 * time.Hour) }},
		{"Mutate_RetainUntil_Past", func(m *SnapshotManifest) { m.WORM.RetainUntil = now.Add(-1 * time.Hour) }},
		{"Mutate_RetainUntil_Future", func(m *SnapshotManifest) { m.WORM.RetainUntil = now.Add(365 * 24 * time.Hour) }},
		{"Mutate_LegalHold_True", func(m *SnapshotManifest) { m.WORM.LegalHold = true }},
		{"Mutate_Mode_Governance", func(m *SnapshotManifest) { m.WORM.Mode = WORMModeGovernance }},
		{"Mutate_Enabled_False", func(m *SnapshotManifest) { m.WORM.Enabled = false }},
		{"Mutate_RetentionDays", func(m *SnapshotManifest) { m.WORM.RetentionDays = 7 }},
		{"Mutate_CorruptHMAC_Byte0", func(m *SnapshotManifest) {
			m.WORM.ManifestHMAC = "0000000000000000000000000000000000000000000000000000000000000000"
		}},
		{"Mutate_EmptyHMAC", func(m *SnapshotManifest) { m.WORM.ManifestHMAC = "" }},
	}

	for _, tc := range tamperCases {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("snap-%s", strings.ToLower(tc.name))
			m := makeBaseManifest(id)

			// Apply mutation without updating HMAC
			tc.mutateFn(m)

			data, _ := json.MarshalIndent(m, "", "  ")
			snapPath := filepath.Join(snapDir, id+".json")
			if err := os.WriteFile(snapPath, data, 0644); err != nil {
				t.Fatalf("WriteFile failed: %v", err)
			}
			defer os.Remove(snapPath)

			// 1. GetSnapshot must mark Tampered = true
			loaded, err := engine.GetSnapshot(id)
			if err != nil {
				t.Fatalf("GetSnapshot error: %v", err)
			}
			if !loaded.Tampered {
				t.Fatalf("Mutation %s was NOT detected as tampered!", tc.name)
			}

			// 2. DeleteSnapshot must return ErrSnapshotWORMLocked
			delErr := engine.DeleteSnapshot(id)
			if delErr == nil {
				t.Fatalf("DeleteSnapshot succeeded on tampered snapshot for mutation %s!", tc.name)
			}
			if !errors.Is(delErr, ErrSnapshotWORMLocked) {
				t.Fatalf("Expected ErrSnapshotWORMLocked, got %v for mutation %s", delErr, tc.name)
			}

			// 3. File must still be present on disk
			if _, statErr := os.Stat(snapPath); statErr != nil {
				t.Fatalf("File was deleted from disk for mutation %s: %v", tc.name, statErr)
			}
		})
	}
}

// TestChallengerR2_DeleteSnapshot_LegitimateNonWORMAllowed verifies that
// DeleteSnapshot operates normally on legitimate non-WORM snapshots.
func TestChallengerR2_DeleteSnapshot_LegitimateNonWORMAllowed(t *testing.T) {
	tempDir := t.TempDir()
	snapDir := filepath.Join(tempDir, "snapshots")
	_ = os.MkdirAll(snapDir, 0755)

	key := "legit-non-worm-key-32b-length!!!"
	now := time.Now().Truncate(time.Second)

	// Node with WORM disabled
	cfg := &config.Config{
		EncryptionKey: key,
		StateDir:      snapDir,
		WORM: config.WORMConfig{
			Enabled: false,
		},
	}

	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}

	snapID := "snap-legit-non-worm-001"
	m := &SnapshotManifest{
		SnapshotID: snapID,
		CreatedAt:  now.Add(-2 * time.Hour),
		NodeName:   "Workstation",
		TotalFiles: 5,
		TotalBytes: 1024,
		WORM: WORMMetadata{
			Enabled: false,
		},
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	snapPath := filepath.Join(snapDir, snapID+".json")
	_ = os.WriteFile(snapPath, data, 0644)

	// Deleting a non-WORM snapshot must succeed
	if err := engine.DeleteSnapshot(snapID); err != nil {
		t.Fatalf("DeleteSnapshot failed on legitimate non-WORM snapshot: %v", err)
	}

	// File should be deleted
	if _, statErr := os.Stat(snapPath); statErr == nil {
		t.Fatalf("File still exists on disk after successful DeleteSnapshot!")
	}
}

// TestChallengerR2_PruneSnapshots_QuarantineProtectsEntireRepo verifies that
// the presence of a single tampered snapshot halts PruneSnapshots completely,
// safeguarding all legitimate snapshots from deletion.
func TestChallengerR2_PruneSnapshots_QuarantineProtectsEntireRepo(t *testing.T) {
	tempDir := t.TempDir()
	snapDir := filepath.Join(tempDir, "snapshots")
	_ = os.MkdirAll(snapDir, 0755)

	key := "quarantine-protects-all-key-32b!"
	now := time.Now().Truncate(time.Second)

	cfg := &config.Config{
		EncryptionKey: key,
		StateDir:      snapDir,
		Retention: config.RetentionConfig{
			KeepHourly: 0,
			KeepDaily:  0,
		},
		WORM: config.WORMConfig{
			Enabled:       true,
			Mode:          "compliance",
			RetentionDays: 30,
		},
	}

	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}

	// Create 5 valid snapshots (some expired non-WORM that would normally be deleted)
	var snapIDs []string
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("snap-valid-%02d", i)
		snapIDs = append(snapIDs, id)
		m := &SnapshotManifest{
			SnapshotID: id,
			CreatedAt:  now.Add(-time.Duration(i*5) * time.Hour),
			NodeName:   "TestNode",
			TotalFiles: 1,
			TotalBytes: 100,
			WORM: WORMMetadata{
				Enabled: false, // Would be candidate for deletion under 0 retention
			},
		}
		data, _ := json.Marshal(m)
		_ = os.WriteFile(filepath.Join(snapDir, id+".json"), data, 0644)
	}

	// Create 1 tampered snapshot (malware flipped enabled: false)
	tamperedID := "snap-tampered-quarantine-01"
	snapIDs = append(snapIDs, tamperedID)
	tamperedManifest := &SnapshotManifest{
		SnapshotID: tamperedID,
		CreatedAt:  now.Add(-30 * time.Minute),
		NodeName:   "TestNode",
		TotalFiles: 10,
		TotalBytes: 500,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      now.Add(-30 * time.Minute),
			RetainUntil:   now.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
		},
	}
	tamperedManifest.WORM.ManifestHMAC = tamperedManifest.ComputeHMAC(key)
	// Now flip enabled: false
	tamperedManifest.WORM.Enabled = false
	tamperedData, _ := json.Marshal(tamperedManifest)
	_ = os.WriteFile(filepath.Join(snapDir, tamperedID+".json"), tamperedData, 0644)

	// Run PruneSnapshots
	plan, pruneErr := engine.PruneSnapshots()
	if pruneErr == nil {
		t.Fatalf("PruneSnapshots did NOT fail when tampered snapshot was present!")
	}
	if len(plan.Delete) != 0 {
		t.Fatalf("PrunePlan included %d items for deletion despite quarantine error!", len(plan.Delete))
	}

	// Verify EVERY snapshot still exists on disk
	for _, id := range snapIDs {
		p := filepath.Join(snapDir, id+".json")
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("Snapshot %s was deleted despite quarantine abort: %v", id, err)
		}
	}
}

// TestChallengerR2_CloudReindex_RejectsTamperedManifests verifies that
// ReindexFromTelegram skips remote manifests where malware altered enabled: false
// or forged HMAC signatures.
func TestChallengerR2_CloudReindex_RejectsTamperedManifests(t *testing.T) {
	key := "cloud-tamper-check-key-32-bytes!"
	now := time.Now().Truncate(time.Second)

	// Create 1 genuine WORM manifest
	mGenuine := &SnapshotManifest{
		SnapshotID: "snap-cloud-genuine-001",
		CreatedAt:  now.Add(-1 * time.Hour),
		NodeName:   "CloudNode",
		TotalFiles: 5,
		TotalBytes: 5000,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      now.Add(-1 * time.Hour),
			RetainUntil:   now.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
		},
		Files: make(map[string]FileEntry),
	}
	mGenuine.WORM.ManifestHMAC = mGenuine.ComputeHMAC(key)
	encGenuine, _ := mGenuine.Encrypt(key)

	// Create 1 tampered manifest where malware flipped enabled: false in cloud document
	mTampered := &SnapshotManifest{
		SnapshotID: "snap-cloud-tampered-002",
		CreatedAt:  now.Add(-2 * time.Hour),
		NodeName:   "CloudNode",
		TotalFiles: 10,
		TotalBytes: 10000,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      now.Add(-2 * time.Hour),
			RetainUntil:   now.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
		},
		Files: make(map[string]FileEntry),
	}
	mTampered.WORM.ManifestHMAC = mTampered.ComputeHMAC(key)
	// Flip enabled: false
	mTampered.WORM.Enabled = false
	encTampered, _ := mTampered.Encrypt(key)

	mux := http.NewServeMux()
	mux.HandleFunc("/bottok/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		offsetStr := r.URL.Query().Get("offset")
		var offset int64
		if offsetStr != "" {
			fmt.Sscanf(offsetStr, "%d", &offset)
		}
		var updates []map[string]interface{}
		if offset <= 1 {
			updates = append(updates, map[string]interface{}{
				"update_id": int64(1),
				"message": map[string]interface{}{
					"message_id": int64(101),
					"caption":    "[TCDRIVE_MANIFEST] snap-cloud-genuine-001 | Files: 5 | Size: 5000",
					"document": map[string]interface{}{
						"file_id":   "fid_genuine",
						"file_name": "snap-cloud-genuine-001.manifest.enc",
					},
				},
			})
			updates = append(updates, map[string]interface{}{
				"update_id": int64(2),
				"message": map[string]interface{}{
					"message_id": int64(102),
					"caption":    "[TCDRIVE_MANIFEST] snap-cloud-tampered-002 | Files: 10 | Size: 10000",
					"document": map[string]interface{}{
						"file_id":   "fid_tampered",
						"file_name": "snap-cloud-tampered-002.manifest.enc",
					},
				},
			})
		}
		resp := map[string]interface{}{"ok": true, "result": updates}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/bottok/getFile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fid := r.URL.Query().Get("file_id")
		resp := map[string]interface{}{
			"ok": true,
			"result": map[string]interface{}{
				"file_id":   fid,
				"file_path": "docs/" + fid,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/file/bottok/docs/fid_genuine", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(encGenuine)
	})

	mux.HandleFunc("/file/bottok/docs/fid_tampered", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(encTampered)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	t.Setenv("TELEGRAM_API_URL", server.URL)

	tempDir := t.TempDir()
	snapDir := filepath.Join(tempDir, "reconstructed")

	cfg := &config.Config{
		BotTokens:     []string{"tok"},
		ChannelID:     "-100123",
		EncryptionKey: key,
		StateDir:      snapDir,
		WORM: config.WORMConfig{
			Enabled: true,
		},
	}

	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}

	summary, err := engine.ReindexFromTelegram(context.Background())
	if err != nil {
		t.Fatalf("ReindexFromTelegram failed: %v", err)
	}

	// Genuine manifest must be restored; tampered manifest must be skipped
	if summary.ManifestsRestored != 1 {
		t.Errorf("Expected 1 restored, got %d", summary.ManifestsRestored)
	}
	if summary.ManifestsSkipped != 1 {
		t.Errorf("Expected 1 skipped, got %d", summary.ManifestsSkipped)
	}

	// Genuine file must exist
	if _, err := os.Stat(filepath.Join(snapDir, "snap-cloud-genuine-001.json")); err != nil {
		t.Errorf("Genuine manifest not restored on disk: %v", err)
	}

	// Tampered file must NOT exist on disk
	if _, err := os.Stat(filepath.Join(snapDir, "snap-cloud-tampered-002.json")); err == nil {
		t.Errorf("Tampered manifest WAS restored to disk when it should have been skipped!")
	}
}
