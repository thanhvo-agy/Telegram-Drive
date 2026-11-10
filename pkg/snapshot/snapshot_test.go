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

func TestDiffAndDirectoryListing(t *testing.T) {
	manifestA := &SnapshotManifest{
		SnapshotID: "snap-20261110-010000",
		CreatedAt:  time.Now().Add(-1 * time.Hour),
		NodeName:   "TestNode",
		Files: map[string]FileEntry{
			"docs/finance.pdf": {
				Path:     "docs/finance.pdf",
				Size:     1024,
				ModTime:  1000,
				FileHash: "hash-finance-v1",
			},
			"photos/trip.jpg": {
				Path:     "photos/trip.jpg",
				Size:     2048,
				ModTime:  1000,
				FileHash: "hash-trip-v1",
			},
			"temp.log": {
				Path:     "temp.log",
				Size:     500,
				ModTime:  1000,
				FileHash: "hash-temp-v1",
			},
		},
	}

	manifestB := &SnapshotManifest{
		SnapshotID: "snap-20261110-020000",
		CreatedAt:  time.Now(),
		NodeName:   "TestNode",
		Files: map[string]FileEntry{
			"docs/finance.pdf": {
				Path:     "docs/finance.pdf",
				Size:     1500,
				ModTime:  2000,
				FileHash: "hash-finance-v2", // MODIFIED
			},
			"photos/trip.jpg": {
				Path:     "photos/trip.jpg",
				Size:     2048,
				ModTime:  1000,
				FileHash: "hash-trip-v1", // UNCHANGED
			},
			"new_file.txt": {
				Path:     "new_file.txt",
				Size:     100,
				ModTime:  2000,
				FileHash: "hash-new-v1", // ADDED
			},
			// temp.log removed -> DELETED
		},
	}

	diffs := DiffManifests(manifestA, manifestB)
	diffMap := make(map[string]DiffStatus)
	for _, d := range diffs {
		diffMap[d.Path] = d.Status
	}

	if diffMap["docs/finance.pdf"] != DiffModified {
		t.Errorf("Expected docs/finance.pdf to be MODIFIED, got %s", diffMap["docs/finance.pdf"])
	}
	if diffMap["photos/trip.jpg"] != DiffUnchanged {
		t.Errorf("Expected photos/trip.jpg to be UNCHANGED, got %s", diffMap["photos/trip.jpg"])
	}
	if diffMap["new_file.txt"] != DiffAdded {
		t.Errorf("Expected new_file.txt to be ADDED, got %s", diffMap["new_file.txt"])
	}
	if diffMap["temp.log"] != DiffDeleted {
		t.Errorf("Expected temp.log to be DELETED, got %s", diffMap["temp.log"])
	}

	// Test File Station Directory Listing
	rootItems := manifestB.ListDirectory("")
	if len(rootItems) == 0 {
		t.Fatal("Expected root items, got 0")
	}

	// Check if docs/ and photos/ are directories
	dirFound := false
	for _, item := range rootItems {
		if item.Name == "docs" && item.IsDir {
			dirFound = true
		}
	}
	if !dirFound {
		t.Error("Expected directory 'docs' in root listing")
	}

	// Subfolder listing for "docs"
	docsItems := manifestB.ListDirectory("docs")
	if len(docsItems) != 1 || docsItems[0].Name != "finance.pdf" {
		t.Errorf("Expected docs listing to contain finance.pdf, got: %+v", docsItems)
	}
}

func TestRetentionPruner(t *testing.T) {
	now := time.Now()
	var snapshots []SnapshotSummary

	// Create 30 hourly snapshots
	for i := 0; i < 30; i++ {
		snapshots = append(snapshots, SnapshotSummary{
			SnapshotID: fmt.Sprintf("snap-%02d", i),
			CreatedAt:  now.Add(-time.Duration(i) * time.Hour),
		})
	}

	policy := config.RetentionConfig{
		KeepHourly:  12,
		KeepDaily:   7,
		KeepWeekly:  2,
		KeepMonthly: 1,
	}

	plan := CalculatePrunePlan(snapshots, policy)
	if len(plan.Keep) == 0 {
		t.Fatal("Expected kept snapshots, got 0")
	}
	if len(plan.Delete) == 0 {
		t.Fatal("Expected deleted snapshots, got 0")
	}
	if len(plan.Keep)+len(plan.Delete) != len(snapshots) {
		t.Errorf("Total snapshots mismatch: %d kept + %d deleted != %d total",
			len(plan.Keep), len(plan.Delete), len(snapshots))
	}
}

func TestDeepNestedAndEmptyDirectories(t *testing.T) {
	manifest := &SnapshotManifest{
		SnapshotID: "snap-nested-test",
		CreatedAt:  time.Now(),
		NodeName:   "NAS-Test",
		Directories: []string{
			"level1",
			"level1/level2",
			"level1/level2/level3",
			"empty_folder",
		},
		Files: map[string]FileEntry{
			"level1/level2/level3/deep_file.txt": {
				Path:     "level1/level2/level3/deep_file.txt",
				Size:     4096,
				ModTime:  1700000000,
				FileHash: "sha256-deep",
			},
		},
	}

	// 1. Root level must contain level1 and empty_folder
	rootItems := manifest.ListDirectory("")
	if len(rootItems) != 2 {
		t.Fatalf("Expected 2 items in root, got %d: %+v", len(rootItems), rootItems)
	}

	// 2. Listing level1 must contain level2
	l1Items := manifest.ListDirectory("level1")
	if len(l1Items) != 1 || !l1Items[0].IsDir || l1Items[0].Name != "level2" {
		t.Fatalf("Expected level2 directory in level1, got %+v", l1Items)
	}

	// 3. Listing level1/level2 must contain level3
	l2Items := manifest.ListDirectory("level1/level2")
	if len(l2Items) != 1 || !l2Items[0].IsDir || l2Items[0].Name != "level3" {
		t.Fatalf("Expected level3 directory in level1/level2, got %+v", l2Items)
	}

	// 4. Listing level1/level2/level3 must contain deep_file.txt
	l3Items := manifest.ListDirectory("level1/level2/level3")
	if len(l3Items) != 1 || l3Items[0].IsDir || l3Items[0].Name != "deep_file.txt" {
		t.Fatalf("Expected deep_file.txt in level3, got %+v", l3Items)
	}

	// 5. Listing empty_folder must be empty
	emptyItems := manifest.ListDirectory("empty_folder")
	if len(emptyItems) != 0 {
		t.Fatalf("Expected empty_folder to have 0 items, got %d", len(emptyItems))
	}
}

func TestWORM_Metadata_Serialization(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	retainUntil := now.Add(90 * 24 * time.Hour)

	m := SnapshotManifest{
		SnapshotID: "snap-20261110-worm-meta",
		CreatedAt:  now,
		NodeName:   "NAS-Primary",
		TotalFiles: 150,
		TotalBytes: 52428800,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      now,
			RetainUntil:   retainUntil,
			RetentionDays: 90,
			LegalHold:     false,
			ManifestHMAC:  "deadbeef12345678",
		},
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal WORM manifest: %v", err)
	}

	var parsed SnapshotManifest
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal WORM manifest: %v", err)
	}

	if !parsed.WORM.Enabled {
		t.Errorf("expected parsed.WORM.Enabled == true")
	}
	if parsed.WORM.Mode != WORMModeCompliance {
		t.Errorf("expected parsed.WORM.Mode == %s, got %s", WORMModeCompliance, parsed.WORM.Mode)
	}
	if parsed.WORM.RetentionDays != 90 {
		t.Errorf("expected parsed.WORM.RetentionDays == 90, got %d", parsed.WORM.RetentionDays)
	}
	if parsed.WORM.LegalHold {
		t.Errorf("expected parsed.WORM.LegalHold == false")
	}
	if parsed.WORM.ManifestHMAC != "deadbeef12345678" {
		t.Errorf("expected parsed.WORM.ManifestHMAC == deadbeef12345678, got %s", parsed.WORM.ManifestHMAC)
	}
	if parsed.WORM.LockedAt.Unix() != now.Unix() {
		t.Errorf("locked_at mismatch: expected %d, got %d", now.Unix(), parsed.WORM.LockedAt.Unix())
	}
	if parsed.WORM.RetainUntil.Unix() != retainUntil.Unix() {
		t.Errorf("retain_until mismatch: expected %d, got %d", retainUntil.Unix(), parsed.WORM.RetainUntil.Unix())
	}
}

func TestWORM_HMAC_Computation_And_Tamper_Detection(t *testing.T) {
	key := "enterprise-worm-master-key-32b!"
	now := time.Now().Truncate(time.Second)
	retainUntil := now.Add(30 * 24 * time.Hour)

	m := SnapshotManifest{
		SnapshotID: "snap-20261110-hmac-test",
		CreatedAt:  now,
		NodeName:   "Production-NAS-01",
		TotalFiles: 42,
		TotalBytes: 1048576,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      now,
			RetainUntil:   retainUntil,
			RetentionDays: 30,
			LegalHold:     false,
		},
	}

	sig := m.ComputeHMAC(key)
	if sig == "" {
		t.Fatalf("ComputeHMAC returned empty signature")
	}
	m.WORM.ManifestHMAC = sig

	// 1. Verify genuine manifest
	if !m.VerifyHMAC(key) {
		t.Fatalf("genuine manifest failed HMAC verification")
	}

	// 2. Tampering with key must fail
	if m.VerifyHMAC("wrong-encryption-key") {
		t.Fatalf("manifest verified with wrong key")
	}

	// 3. Tampering with retain_until (e.g. ransomware shortening retention)
	tamperedRetain := m
	tamperedRetain.WORM.RetainUntil = now.Add(-1 * time.Hour)
	if tamperedRetain.VerifyHMAC(key) {
		t.Fatalf("tampered retain_until was not detected by VerifyHMAC")
	}

	// 4. Tampering with legal hold
	tamperedHold := m
	tamperedHold.WORM.LegalHold = true
	if tamperedHold.VerifyHMAC(key) {
		t.Fatalf("tampered legal_hold was not detected by VerifyHMAC")
	}

	// 5. Tampering with total files or bytes
	tamperedSize := m
	tamperedSize.TotalBytes = 99999999
	if tamperedSize.VerifyHMAC(key) {
		t.Fatalf("tampered total_bytes was not detected by VerifyHMAC")
	}

	// 6. Tampering with mode
	tamperedMode := m
	tamperedMode.WORM.Mode = WORMModeGovernance
	if tamperedMode.VerifyHMAC(key) {
		t.Fatalf("tampered mode was not detected by VerifyHMAC")
	}

	// 7. Tampering with enabled (setting enabled: false)
	tamperedEnabled := m
	tamperedEnabled.WORM.Enabled = false
	if tamperedEnabled.VerifyHMAC(key) {
		t.Fatalf("tampered enabled: false was not detected by VerifyHMAC")
	}

	// 8. Tampering with retention_days
	tamperedDays := m
	tamperedDays.WORM.RetentionDays = 100
	if tamperedDays.VerifyHMAC(key) {
		t.Fatalf("tampered retention_days was not detected by VerifyHMAC")
	}
}

func TestWORM_IsWORMLocked_Rules(t *testing.T) {
	now := time.Now()
	past := now.Add(-24 * time.Hour)
	future := now.Add(24 * time.Hour)

	// Case 1: Disabled WORM
	m1 := SnapshotManifest{WORM: WORMMetadata{Enabled: false, RetainUntil: future}}
	if m1.IsWORMLocked(now) {
		t.Errorf("disabled WORM should return false")
	}

	// Case 2: Enabled WORM with future RetainUntil
	m2 := SnapshotManifest{WORM: WORMMetadata{Enabled: true, RetainUntil: future}}
	if !m2.IsWORMLocked(now) {
		t.Errorf("enabled WORM with future RetainUntil should return true")
	}

	// Case 3: Enabled WORM with past RetainUntil, no LegalHold
	m3 := SnapshotManifest{WORM: WORMMetadata{Enabled: true, RetainUntil: past, LegalHold: false}}
	if m3.IsWORMLocked(now) {
		t.Errorf("expired WORM with no legal hold should return false")
	}

	// Case 4: Enabled WORM with past RetainUntil, BUT LegalHold active
	m4 := SnapshotManifest{WORM: WORMMetadata{Enabled: true, RetainUntil: past, LegalHold: true}}
	if !m4.IsWORMLocked(now) {
		t.Errorf("expired WORM with LegalHold=true MUST return true")
	}

	// Verify same rules on SnapshotSummary
	sum := m4.Summary()
	if !sum.IsWORMLocked(now) {
		t.Errorf("SnapshotSummary with LegalHold=true MUST return true for IsWORMLocked")
	}
}

func TestWORM_CalculatePrunePlan_Lock_Enforcement(t *testing.T) {
	now := time.Now()
	policy := config.RetentionConfig{
		KeepHourly:  1,
		KeepDaily:   1,
		KeepWeekly:  1,
		KeepMonthly: 1,
	}

	// Create 10 snapshots spaced out
	var snapshots []SnapshotSummary
	for i := 0; i < 10; i++ {
		snapTime := now.Add(-time.Duration(i*24) * time.Hour)
		sum := SnapshotSummary{
			SnapshotID: fmt.Sprintf("snap-day-%d", i),
			CreatedAt:  snapTime,
		}
		// Mark snap-day-5 and snap-day-8 with active WORM lock
		if i == 5 {
			sum.WORMEnabled = true
			sum.WORMLocked = true
			sum.WORMRetainUntil = now.Add(30 * 24 * time.Hour)
		}
		if i == 8 {
			sum.WORMEnabled = true
			sum.WORMLocked = true
			sum.WORMLegalHold = true
		}
		snapshots = append(snapshots, sum)
	}

	plan := CalculatePrunePlanAt(snapshots, policy, now)

	// Ensure snap-day-5 and snap-day-8 are in Keep and NEVER in Delete
	for _, del := range plan.Delete {
		if del.SnapshotID == "snap-day-5" {
			t.Fatalf("WORM-locked snapshot snap-day-5 was placed in Delete list!")
		}
		if del.SnapshotID == "snap-day-8" {
			t.Fatalf("Legal-hold snapshot snap-day-8 was placed in Delete list!")
		}
	}

	keptMap := make(map[string]bool)
	for _, kept := range plan.Keep {
		keptMap[kept.SnapshotID] = true
	}

	if !keptMap["snap-day-5"] {
		t.Errorf("expected snap-day-5 to be in Keep list")
	}
	if !keptMap["snap-day-8"] {
		t.Errorf("expected snap-day-8 to be in Keep list")
	}
}

func TestWORM_Engine_Prune_And_Delete_Guards(t *testing.T) {
	tempDir := t.TempDir()
	snapDir := filepath.Join(tempDir, "snapshots")
	_ = os.MkdirAll(snapDir, 0755)

	key := "test-worm-encryption-key-32b"
	cfg := &config.Config{
		EncryptionKey: key,
		StateDir:      snapDir,
		Retention: config.RetentionConfig{
			KeepHourly: 1,
		},
		WORM: config.WORMConfig{
			Enabled:       true,
			Mode:          "compliance",
			RetentionDays: 30,
		},
	}

	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("failed to initialize Engine: %v", err)
	}

	now := time.Now()
	// Create a locked snapshot
	lockedSnap := &SnapshotManifest{
		SnapshotID: "snap-locked-001",
		CreatedAt:  now.Add(-2 * time.Hour),
		NodeName:   "TestNode",
		TotalFiles: 1,
		TotalBytes: 100,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      now,
			RetainUntil:   now.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
		},
	}
	lockedSnap.WORM.ManifestHMAC = lockedSnap.ComputeHMAC(key)
	data, _ := json.MarshalIndent(lockedSnap, "", "  ")
	_ = os.WriteFile(filepath.Join(snapDir, lockedSnap.SnapshotID+".json"), data, 0644)

	// Attempting to DeleteSnapshot on locked snapshot must fail
	err = engine.DeleteSnapshot("snap-locked-001")
	if err == nil {
		t.Fatalf("expected DeleteSnapshot to fail on WORM-locked snapshot")
	}
	if !errors.Is(err, ErrSnapshotWORMLocked) {
		t.Errorf("expected error to wrap ErrSnapshotWORMLocked, got: %v", err)
	}

	// Verify file is still intact on disk
	if _, err := os.Stat(filepath.Join(snapDir, "snap-locked-001.json")); err != nil {
		t.Errorf("locked snapshot file was deleted despite guard: %v", err)
	}
}

func TestWORM_Quarantine_Tampered_Manifest(t *testing.T) {
	tempDir := t.TempDir()
	snapDir := filepath.Join(tempDir, "snapshots")
	_ = os.MkdirAll(snapDir, 0755)

	key := "test-worm-encryption-key-32b"
	cfg := &config.Config{
		EncryptionKey: key,
		StateDir:      snapDir,
	}

	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("failed to initialize Engine: %v", err)
	}

	now := time.Now()
	tamperedSnap := &SnapshotManifest{
		SnapshotID: "snap-tampered-001",
		CreatedAt:  now,
		NodeName:   "TestNode",
		TotalFiles: 10,
		TotalBytes: 5000,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      now,
			RetainUntil:   now.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
			ManifestHMAC:  "forged_or_invalid_hmac_signature",
		},
	}

	data, _ := json.MarshalIndent(tamperedSnap, "", "  ")
	_ = os.WriteFile(filepath.Join(snapDir, tamperedSnap.SnapshotID+".json"), data, 0644)

	// 1. LoadSnapshot must return ErrTamperedManifest
	loaded, err := engine.LoadSnapshot("snap-tampered-001")
	if err == nil {
		t.Fatalf("expected LoadSnapshot to fail on tampered manifest")
	}
	if !errors.Is(err, ErrTamperedManifest) {
		t.Errorf("expected ErrTamperedManifest, got %v", err)
	}
	if loaded == nil || !loaded.Tampered {
		t.Errorf("expected loaded.Tampered == true")
	}

	// 2. PruneSnapshots must abort immediately with error if a tampered snapshot is present
	_, err = engine.PruneSnapshots()
	if err == nil {
		t.Fatalf("expected PruneSnapshots to abort when tampered snapshot exists")
	}
	if !strings.Contains(err.Error(), "tampered snapshot detected") {
		t.Errorf("unexpected prune error message: %v", err)
	}

	// 3. DeleteSnapshot must reject tampered snapshot
	delErr := engine.DeleteSnapshot("snap-tampered-001")
	if delErr == nil {
		t.Fatalf("expected DeleteSnapshot to fail on tampered snapshot")
	}
	if !errors.Is(delErr, ErrSnapshotWORMLocked) {
		t.Errorf("expected DeleteSnapshot to wrap ErrSnapshotWORMLocked, got %v", delErr)
	}
}

func TestWORM_Clock_Skew_Detection(t *testing.T) {
	local := time.Date(2028, 1, 1, 12, 0, 0, 0, time.UTC)
	remote := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	detected, skew := CheckClockSkew(local, remote, 1*time.Hour)
	if !detected {
		t.Errorf("expected clock skew anomaly to be detected")
	}
	if skew < 700*24*time.Hour {
		t.Errorf("expected ~2 years skew, got %v", skew)
	}

	// Normal sync (e.g. 5 seconds skew)
	syncedLocal := time.Date(2026, 1, 1, 12, 0, 5, 0, time.UTC)
	syncedDetected, _ := CheckClockSkew(syncedLocal, remote, 1*time.Hour)
	if syncedDetected {
		t.Errorf("small 5-second skew should not trigger alert")
	}

	// Test PruneSnapshots clock skew validation
	tempDir := t.TempDir()
	snapDir := filepath.Join(tempDir, "snapshots")
	_ = os.MkdirAll(snapDir, 0755)

	cfg := &config.Config{
		StateDir: snapDir,
	}
	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("failed to init engine: %v", err)
	}

	// Case A: Backwards clock shift check (snapshot from future)
	futureSnap := &SnapshotManifest{
		SnapshotID: "snap-future-001",
		CreatedAt:  time.Now().Add(2 * time.Hour),
		NodeName:   "TestNode",
	}
	data, _ := json.Marshal(futureSnap)
	_ = os.WriteFile(filepath.Join(snapDir, "snap-future-001.json"), data, 0644)

	_, pruneErr := engine.PruneSnapshots()
	if pruneErr == nil {
		t.Fatalf("expected PruneSnapshots to abort on backwards clock shift")
	}
	if !strings.Contains(pruneErr.Error(), "clock skew backwards") {
		t.Errorf("expected 'clock skew backwards' error, got: %v", pruneErr)
	}
	_ = os.Remove(filepath.Join(snapDir, "snap-future-001.json"))

	// Case B: Remote clock skew check (> 1 hour difference)
	validSnap := &SnapshotManifest{
		SnapshotID: "snap-valid-001",
		CreatedAt:  time.Now().Add(-10 * time.Minute),
		NodeName:   "TestNode",
	}
	dataValid, _ := json.Marshal(validSnap)
	_ = os.WriteFile(filepath.Join(snapDir, "snap-valid-001.json"), dataValid, 0644)

	engine.SetRemoteTimeFunc(func(ctx context.Context) (time.Time, error) {
		return time.Now().Add(-3 * time.Hour), nil // 3 hours in the past
	})
	_, pruneErr = engine.PruneSnapshots()
	if pruneErr == nil {
		t.Fatalf("expected PruneSnapshots to abort on remote clock skew > 1h")
	}
	if !strings.Contains(pruneErr.Error(), "clock skew detected between host and remote") {
		t.Errorf("expected remote clock skew error, got: %v", pruneErr)
	}
}

func TestWORM_Disaster_Recovery_Reindex_Full_Flow(t *testing.T) {
	key := "master-disaster-recovery-key-32b"
	now := time.Now().Truncate(time.Second)

	// Create genuine snapshot manifest with WORM
	manifest := &SnapshotManifest{
		SnapshotID: "snap-20261110-dr-test",
		CreatedAt:  now,
		NodeName:   "DisasterRecoveryNode",
		TotalFiles: 5,
		TotalBytes: 20480,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      now,
			RetainUntil:   now.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
		},
		Files: make(map[string]FileEntry),
	}
	manifest.WORM.ManifestHMAC = manifest.ComputeHMAC(key)

	encManifest, err := manifest.Encrypt(key)
	if err != nil {
		t.Fatalf("failed to encrypt manifest: %v", err)
	}

	// Mock Telegram server
	mux := http.NewServeMux()
	mux.HandleFunc("/botmocktoken/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		offsetStr := r.URL.Query().Get("offset")
		var offset int64
		if offsetStr != "" {
			fmt.Sscanf(offsetStr, "%d", &offset)
		}
		var updates []map[string]interface{}
		if offset <= 100 {
			updates = append(updates, map[string]interface{}{
				"update_id": 100,
				"message": map[string]interface{}{
					"message_id": 999,
					"caption":    "[TCDRIVE_MANIFEST] snap-20261110-dr-test | Files: 5 | Size: 20480",
					"document": map[string]interface{}{
						"file_id":   "file_manifest_dr",
						"file_name": "snap-20261110-dr-test.manifest.enc",
					},
				},
			})
		}
		resp := map[string]interface{}{
			"ok":     true,
			"result": updates,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/botmocktoken/getFile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"ok": true,
			"result": map[string]interface{}{
				"file_id":   "file_manifest_dr",
				"file_path": "documents/file_manifest_dr",
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/file/botmocktoken/documents/file_manifest_dr", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(encManifest)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	t.Setenv("TELEGRAM_API_URL", server.URL)

	emptySnapDir := filepath.Join(t.TempDir(), "recovered_snapshots")
	cfg := &config.Config{
		BotTokens:     []string{"mocktoken"},
		ChannelID:     "-100123456789",
		EncryptionKey: key,
		StateDir:      emptySnapDir,
	}

	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("failed to init engine: %v", err)
	}

	summary, err := engine.ReindexFromTelegram(context.Background())
	if err != nil {
		t.Fatalf("ReindexFromTelegram failed: %v", err)
	}

	if summary.ManifestsRestored != 1 {
		t.Errorf("expected 1 manifest restored, got %d", summary.ManifestsRestored)
	}
	if summary.TotalFilesRestored != 5 {
		t.Errorf("expected 5 files restored, got %d", summary.TotalFilesRestored)
	}
	if summary.TotalBytesRestored != 20480 {
		t.Errorf("expected 20480 bytes restored, got %d", summary.TotalBytesRestored)
	}

	// Verify local file exists and can be loaded
	recovered, err := engine.GetSnapshot("snap-20261110-dr-test")
	if err != nil {
		t.Fatalf("failed to retrieve recovered snapshot: %v", err)
	}
	if recovered.Tampered {
		t.Errorf("recovered snapshot was mistakenly marked tampered")
	}
	if !recovered.IsWORMLocked(time.Now()) {
		t.Errorf("expected recovered snapshot to be WORM locked")
	}
}

func TestWORM_Disaster_Recovery_Reindex_ErrorHandling(t *testing.T) {
	// Case 1: HTTP 401 Unauthorized
	server401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	}))
	defer server401.Close()

	t.Setenv("TELEGRAM_API_URL", server401.URL)
	cfg := &config.Config{
		BotTokens:     []string{"badtoken"},
		ChannelID:     "-100123456789",
		EncryptionKey: "key-32-bytes-long-for-testing!!",
		StateDir:      t.TempDir(),
	}
	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("failed to init engine: %v", err)
	}

	_, err = engine.ReindexFromTelegram(context.Background())
	if err == nil {
		t.Fatalf("expected ReindexFromTelegram to return error on HTTP 401")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("expected error to mention HTTP 401, got: %v", err)
	}

	// Case 2: HTTP 429 Rate Limit
	server429 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests"}`))
	}))
	defer server429.Close()

	t.Setenv("TELEGRAM_API_URL", server429.URL)
	_, err = engine.ReindexFromTelegram(context.Background())
	if err == nil {
		t.Fatalf("expected ReindexFromTelegram to return error on HTTP 429")
	}
	if !strings.Contains(err.Error(), "HTTP 429") {
		t.Errorf("expected error to mention HTTP 429, got: %v", err)
	}

	// Case 3: Invalid JSON body
	serverMalformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer serverMalformed.Close()

	t.Setenv("TELEGRAM_API_URL", serverMalformed.URL)
	_, err = engine.ReindexFromTelegram(context.Background())
	if err == nil {
		t.Fatalf("expected ReindexFromTelegram to return error on malformed JSON")
	}
}



