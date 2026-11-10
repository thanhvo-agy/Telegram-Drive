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

// TestAdversarial_TamperDetection_AllCanonicalFields systematically mutates
// every single field in CanonicalString to verify that HMAC fails on each.
func TestAdversarial_TamperDetection_AllCanonicalFields(t *testing.T) {
	key := "adv-secret-key-32-bytes-length!!"
	baseTime := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	retainUntil := baseTime.Add(30 * 24 * time.Hour)

	baseManifest := SnapshotManifest{
		SnapshotID: "snap-20261110-100000",
		CreatedAt:  baseTime,
		NodeName:   "Primary-NAS",
		TotalFiles: 100,
		TotalBytes: 10485760,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      baseTime,
			RetainUntil:   retainUntil,
			RetentionDays: 30,
			LegalHold:     false,
		},
	}
	baseManifest.WORM.ManifestHMAC = baseManifest.ComputeHMAC(key)

	if !baseManifest.VerifyHMAC(key) {
		t.Fatalf("Base manifest should be valid")
	}

	testCases := []struct {
		name     string
		mutateFn func(m *SnapshotManifest)
	}{
		{
			name: "Mutate SnapshotID",
			mutateFn: func(m *SnapshotManifest) {
				m.SnapshotID = "snap-tampered-id"
			},
		},
		{
			name: "Mutate CreatedAt",
			mutateFn: func(m *SnapshotManifest) {
				m.CreatedAt = m.CreatedAt.Add(1 * time.Hour)
			},
		},
		{
			name: "Mutate NodeName",
			mutateFn: func(m *SnapshotManifest) {
				m.NodeName = "Malicious-NAS"
			},
		},
		{
			name: "Mutate TotalFiles",
			mutateFn: func(m *SnapshotManifest) {
				m.TotalFiles = m.TotalFiles + 1
			},
		},
		{
			name: "Mutate TotalBytes",
			mutateFn: func(m *SnapshotManifest) {
				m.TotalBytes = m.TotalBytes - 1024
			},
		},
		{
			name: "Mutate LockedAt",
			mutateFn: func(m *SnapshotManifest) {
				m.WORM.LockedAt = m.WORM.LockedAt.Add(-24 * time.Hour)
			},
		},
		{
			name: "Mutate RetainUntil (shorten)",
			mutateFn: func(m *SnapshotManifest) {
				m.WORM.RetainUntil = m.WORM.RetainUntil.Add(-10 * 24 * time.Hour)
			},
		},
		{
			name: "Mutate RetainUntil (extend)",
			mutateFn: func(m *SnapshotManifest) {
				m.WORM.RetainUntil = m.WORM.RetainUntil.Add(10 * 24 * time.Hour)
			},
		},
		{
			name: "Mutate LegalHold (false -> true)",
			mutateFn: func(m *SnapshotManifest) {
				m.WORM.LegalHold = true
			},
		},
		{
			name: "Mutate Mode (compliance -> governance)",
			mutateFn: func(m *SnapshotManifest) {
				m.WORM.Mode = WORMModeGovernance
			},
		},
		{
			name: "Mutate Enabled (true -> false)",
			mutateFn: func(m *SnapshotManifest) {
				m.WORM.Enabled = false
			},
		},
		{
			name: "Mutate RetentionDays",
			mutateFn: func(m *SnapshotManifest) {
				m.WORM.RetentionDays = 999
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := baseManifest
			tc.mutateFn(&mutated)
			if mutated.VerifyHMAC(key) {
				t.Errorf("[%s] VerifyHMAC returned true for tampered field!", tc.name)
			}
		})
	}
}

// TestAdversarial_TamperDetection_HMACBitFlips flips bits and alters bytes in the HMAC signature.
func TestAdversarial_TamperDetection_HMACBitFlips(t *testing.T) {
	key := "adv-secret-key-32-bytes-length!!"
	baseTime := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)

	m := SnapshotManifest{
		SnapshotID: "snap-20261110-100000",
		CreatedAt:  baseTime,
		NodeName:   "Primary-NAS",
		TotalFiles: 10,
		TotalBytes: 5000,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      baseTime,
			RetainUntil:   baseTime.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
		},
	}
	originalHMAC := m.ComputeHMAC(key)
	m.WORM.ManifestHMAC = originalHMAC

	if !m.VerifyHMAC(key) {
		t.Fatalf("Original HMAC should verify")
	}

	// Flip 1 character at each position
	for i := 0; i < len(originalHMAC); i++ {
		corrupted := []byte(originalHMAC)
		if corrupted[i] == 'a' {
			corrupted[i] = 'b'
		} else {
			corrupted[i] = 'a'
		}
		m.WORM.ManifestHMAC = string(corrupted)
		if m.VerifyHMAC(key) {
			t.Fatalf("Bit flip at index %d was NOT detected!", i)
		}
	}

	// Truncated HMAC
	m.WORM.ManifestHMAC = originalHMAC[:len(originalHMAC)-4]
	if m.VerifyHMAC(key) {
		t.Fatalf("Truncated HMAC was NOT detected!")
	}

	// Empty HMAC
	m.WORM.ManifestHMAC = ""
	if m.VerifyHMAC(key) {
		t.Fatalf("Empty HMAC was NOT detected!")
	}

	// Random garbage HMAC
	m.WORM.ManifestHMAC = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if m.VerifyHMAC(key) {
		t.Fatalf("Random HMAC was NOT detected!")
	}
}

// TestAdversarial_SimulateMalware_JSONOnDiskTampering creates real JSON files on disk
// and simulates malware directly modifying files.
func TestAdversarial_SimulateMalware_JSONOnDiskTampering(t *testing.T) {
	tempDir := t.TempDir()
	snapDir := filepath.Join(tempDir, "snapshots")
	_ = os.MkdirAll(snapDir, 0755)

	key := "enterprise-malware-test-key-32b"
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
		t.Fatalf("NewEngine failed: %v", err)
	}

	now := time.Now().Truncate(time.Second)

	// Create genuine snapshot
	snap := &SnapshotManifest{
		SnapshotID: "snap-malware-target-001",
		CreatedAt:  now.Add(-2 * time.Hour),
		NodeName:   "NAS-Corp",
		TotalFiles: 50,
		TotalBytes: 1048576,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      now.Add(-2 * time.Hour),
			RetainUntil:   now.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
			LegalHold:     false,
		},
		Files: map[string]FileEntry{
			"finance/q4.xlsx": {
				Path:     "finance/q4.xlsx",
				Size:     1024,
				ModTime:  now.Unix(),
				FileHash: "orig-hash-12345",
			},
		},
	}
	snap.WORM.ManifestHMAC = snap.ComputeHMAC(key)

	snapPath := filepath.Join(snapDir, snap.SnapshotID+".json")
	data, _ := json.MarshalIndent(snap, "", "  ")
	if err := os.WriteFile(snapPath, data, 0644); err != nil {
		t.Fatalf("Failed to write snapshot: %v", err)
	}

	// 1. Attack Scenario A: Malware modifies retain_until to past date in JSON
	t.Run("Malware_Modifies_RetainUntil_In_JSON", func(t *testing.T) {
		raw, err := os.ReadFile(snapPath)
		if err != nil {
			t.Fatalf("Read file failed: %v", err)
		}
		// Replace retain_until with 2020 date
		pastDate := "2020-01-01T00:00:00Z"
		tamperedRaw := strings.Replace(string(raw), snap.WORM.RetainUntil.Format(time.RFC3339Nano), pastDate, 1)
		if !strings.Contains(tamperedRaw, pastDate) {
			tamperedRaw = strings.Replace(string(raw), snap.WORM.RetainUntil.Format("2006-01-02T15:04:05"), "2020-01-01T00:00:00", 1)
		}
		_ = os.WriteFile(snapPath, []byte(tamperedRaw), 0644)

		// Verification: GetSnapshot must detect tampering
		loaded, err := engine.GetSnapshot(snap.SnapshotID)
		if err != nil {
			t.Fatalf("GetSnapshot returned error: %v", err)
		}
		if !loaded.Tampered {
			t.Errorf("FAIL: Snapshot was NOT marked tampered after retain_until modification!")
		}

		// LoadSnapshot must return ErrTamperedManifest
		_, err = engine.LoadSnapshot(snap.SnapshotID)
		if !errors.Is(err, ErrTamperedManifest) {
			t.Errorf("FAIL: Expected ErrTamperedManifest, got %v", err)
		}

		// ListSnapshots must reflect Tampered = true
		list, err := engine.ListSnapshots()
		if err != nil {
			t.Fatalf("ListSnapshots failed: %v", err)
		}
		if len(list) == 0 || !list[0].Tampered {
			t.Errorf("FAIL: ListSnapshots did not mark snapshot tampered")
		}

		// PruneSnapshots must abort immediately and protect snapshot
		_, err = engine.PruneSnapshots()
		if err == nil {
			t.Errorf("FAIL: PruneSnapshots should abort when tampered snapshot is present!")
		}
		if !strings.Contains(err.Error(), "tampered snapshot detected") {
			t.Errorf("FAIL: Expected tampered alert message in prune error, got: %v", err)
		}

		// Verify snapshot file still exists on disk
		if _, err := os.Stat(snapPath); err != nil {
			t.Errorf("FAIL: Snapshot file was deleted from disk: %v", err)
		}

		// Restore genuine file for next subtest
		_ = os.WriteFile(snapPath, data, 0644)
	})

	// 2. Attack Scenario B: Malware flips bits in manifest_hmac on disk
	t.Run("Malware_Corrupts_HMAC_On_Disk", func(t *testing.T) {
		raw, err := os.ReadFile(snapPath)
		if err != nil {
			t.Fatalf("Read file failed: %v", err)
		}
		tamperedRaw := strings.Replace(string(raw), snap.WORM.ManifestHMAC, "deadbeefcafebabe0000111122223333444455556666777788889999aaaabbbb", 1)
		_ = os.WriteFile(snapPath, []byte(tamperedRaw), 0644)

		loaded, err := engine.GetSnapshot(snap.SnapshotID)
		if err != nil {
			t.Fatalf("GetSnapshot returned error: %v", err)
		}
		if !loaded.Tampered {
			t.Errorf("FAIL: Snapshot was NOT marked tampered after HMAC corruption!")
		}

		// Restore genuine file
		_ = os.WriteFile(snapPath, data, 0644)
	})

	// 3. Attack Scenario C: Malware disables WORM by flipping "enabled": false
	t.Run("Malware_Bypasses_By_Setting_Enabled_False", func(t *testing.T) {
		// Add a newer snapshot so there are > 1 snapshots
		newerSnap := &SnapshotManifest{
			SnapshotID: "snap-malware-target-002",
			CreatedAt:  now.Add(1 * time.Minute),
			NodeName:   "NAS-Corp",
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
		newerSnap.WORM.ManifestHMAC = newerSnap.ComputeHMAC(key)
		newerData, _ := json.MarshalIndent(newerSnap, "", "  ")
		newerPath := filepath.Join(snapDir, newerSnap.SnapshotID+".json")
		_ = os.WriteFile(newerPath, newerData, 0644)
		defer os.Remove(newerPath)

		raw, err := os.ReadFile(snapPath)
		if err != nil {
			t.Fatalf("Read file failed: %v", err)
		}

		// Malware sets "enabled": false AND shortens retain_until
		tamperedRaw := strings.Replace(string(raw), `"enabled": true`, `"enabled": false`, 1)
		pastDate := "2020-01-01T00:00:00Z"
		tamperedRaw = strings.Replace(tamperedRaw, snap.WORM.RetainUntil.Format(time.RFC3339Nano), pastDate, 1)
		_ = os.WriteFile(snapPath, []byte(tamperedRaw), 0644)

		loaded, err := engine.GetSnapshot(snap.SnapshotID)
		if err != nil {
			t.Fatalf("GetSnapshot returned error: %v", err)
		}

		t.Logf("Adversarial Evaluation of 'enabled: false' attack:")
		t.Logf("  loaded.WORM.Enabled: %v", loaded.WORM.Enabled)
		t.Logf("  loaded.Tampered: %v", loaded.Tampered)
		t.Logf("  loaded.IsWORMLocked: %v", loaded.IsWORMLocked(time.Now()))

		plan, pruneErr := engine.PruneSnapshots()
		t.Logf("  PruneSnapshots error: %v", pruneErr)
		t.Logf("  PrunePlan.Delete count: %d", len(plan.Delete))

		_, statErr := os.Stat(snapPath)
		t.Logf("  File exists after prune: %v", statErr == nil)

		if !loaded.Tampered {
			t.Fatalf("[VULNERABILITY] When malware modifies 'enabled': false, verifyManifestIntegrity failed to flag it as Tampered!")
		}
		if pruneErr == nil {
			t.Fatalf("[VULNERABILITY] PruneSnapshots succeeded when tampered snapshot exists; expected error!")
		}
		if statErr != nil {
			t.Fatalf("[VULNERABILITY] snap-malware-target-001 was DELETED by PruneSnapshots!")
		}

		// Also verify DeleteSnapshot cannot delete tampered snapshot
		delErr := engine.DeleteSnapshot(snap.SnapshotID)
		if delErr == nil {
			t.Fatalf("[VULNERABILITY] DeleteSnapshot succeeded on tampered snapshot; expected ErrSnapshotWORMLocked")
		}
		if !errors.Is(delErr, ErrSnapshotWORMLocked) {
			t.Fatalf("expected ErrSnapshotWORMLocked, got %v", delErr)
		}

		// Restore genuine file
		_ = os.WriteFile(snapPath, data, 0644)
	})

	// 4. Attack Scenario D: Malware modifies FileEntry contents in manifest JSON
	t.Run("Malware_Modifies_File_Entry_In_Manifest", func(t *testing.T) {
		raw, err := os.ReadFile(snapPath)
		if err != nil {
			t.Fatalf("Read file failed: %v", err)
		}

		tamperedRaw := strings.Replace(string(raw), "orig-hash-12345", "corrupted-hash-bad", 1)
		_ = os.WriteFile(snapPath, []byte(tamperedRaw), 0644)

		loaded, err := engine.GetSnapshot(snap.SnapshotID)
		if err != nil {
			t.Fatalf("GetSnapshot error: %v", err)
		}

		t.Logf("Adversarial Evaluation of FileEntry tampering:")
		t.Logf("  loaded.Tampered: %v", loaded.Tampered)
		if !loaded.Tampered {
			t.Logf("[OBSERVATION] Manifest HMAC signs canonical fields (ID, CreatedAt, Node, TotalFiles, TotalBytes, LockedAt, RetainUntil, LegalHold, Mode). Individual FileEntry map entries are not part of CanonicalString.")
		}

		// Restore genuine file
		_ = os.WriteFile(snapPath, data, 0644)
	})
}

// TestAdversarial_ClockSkew_RansomwareTimeShift simulates malware advancing the host system clock
// into the future to force active WORM locks to appear expired to the pruner.
func TestAdversarial_ClockSkew_RansomwareTimeShift(t *testing.T) {
	tempDir := t.TempDir()
	snapDir := filepath.Join(tempDir, "snapshots")
	_ = os.MkdirAll(snapDir, 0755)

	key := "clock-skew-test-key-32-bytes!!!"
	realTime := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	fakeFutureTime := realTime.Add(5 * 365 * 24 * time.Hour) // 5 years later

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
		t.Fatalf("Failed to initialize Engine: %v", err)
	}

	// Create 2 snapshots: 1 new, 1 older but within 30-day WORM retention
	snapOld := &SnapshotManifest{
		SnapshotID: "snap-worm-protected-older",
		CreatedAt:  realTime.Add(-2 * time.Hour),
		NodeName:   "Production-NAS",
		TotalFiles: 10,
		TotalBytes: 5000,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      realTime.Add(-2 * time.Hour),
			RetainUntil:   realTime.Add(30 * 24 * time.Hour), // Still active for 30 days
			RetentionDays: 30,
		},
	}
	snapOld.WORM.ManifestHMAC = snapOld.ComputeHMAC(key)
	dataOld, _ := json.MarshalIndent(snapOld, "", "  ")
	pathOld := filepath.Join(snapDir, snapOld.SnapshotID+".json")
	_ = os.WriteFile(pathOld, dataOld, 0644)

	snapNew := &SnapshotManifest{
		SnapshotID: "snap-worm-protected-newer",
		CreatedAt:  realTime,
		NodeName:   "Production-NAS",
		TotalFiles: 10,
		TotalBytes: 5000,
		WORM: WORMMetadata{
			Enabled:       true,
			Mode:          WORMModeCompliance,
			LockedAt:      realTime,
			RetainUntil:   realTime.Add(30 * 24 * time.Hour),
			RetentionDays: 30,
		},
	}
	snapNew.WORM.ManifestHMAC = snapNew.ComputeHMAC(key)
	dataNew, _ := json.MarshalIndent(snapNew, "", "  ")
	pathNew := filepath.Join(snapDir, snapNew.SnapshotID+".json")
	_ = os.WriteFile(pathNew, dataNew, 0644)

	// In realTime, snapOld is WORM locked
	if !snapOld.IsWORMLocked(realTime) {
		t.Fatalf("snapOld should be WORM locked at realTime")
	}

	// In fakeFutureTime (clock shifted forward by malware), snapOld appears unlocked!
	if snapOld.IsWORMLocked(fakeFutureTime) {
		t.Fatalf("snapOld should appear expired at fakeFutureTime")
	}

	// Calculate plan at fakeFutureTime
	snapshots, _ := engine.ListSnapshots()
	planFuture := CalculatePrunePlanAt(snapshots, cfg.Retention, fakeFutureTime)

	t.Logf("Clock Shift Prune Evaluation at fake future time (+5 years):")
	t.Logf("  Keeping count: %d", len(planFuture.Keep))
	t.Logf("  Delete count: %d", len(planFuture.Delete))
	for _, del := range planFuture.Delete {
		t.Logf("  Marked for deletion under fake clock: %s", del.SnapshotID)
	}

	if hasSnapshot(planFuture.Delete, "snap-worm-protected-older") {
		t.Logf("[VULNERABILITY CONFIRMED] Advancing host system clock causes CalculatePrunePlan to classify active WORM snapshots as expired and schedule them for deletion, because PruneSnapshots does not validate clock skew against remote trusted time.")
	}
}

// TestAdversarial_WORMRetentionPruner_ComprehensiveMatrix stresses CalculatePrunePlanAt
// across edge cases: legal hold, boundary timestamps, zero retention, alternating locks.
func TestAdversarial_WORMRetentionPruner_ComprehensiveMatrix(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	policy := config.RetentionConfig{
		KeepHourly:  1,
		KeepDaily:   1,
		KeepWeekly:  1,
		KeepMonthly: 1,
	}

	t.Run("ZeroRetentionPolicy_PreservesAllWORMSnapshots", func(t *testing.T) {
		zeroPolicy := config.RetentionConfig{
			KeepHourly:  0,
			KeepDaily:   0,
			KeepWeekly:  0,
			KeepMonthly: 0,
		}

		snapshots := []SnapshotSummary{
			{
				SnapshotID:      "snap-latest",
				CreatedAt:       now,
				WORMEnabled:     true,
				WORMLocked:      true,
				WORMRetainUntil: now.Add(24 * time.Hour),
			},
			{
				SnapshotID:      "snap-locked-1",
				CreatedAt:       now.Add(-2 * time.Hour),
				WORMEnabled:     true,
				WORMLocked:      true,
				WORMRetainUntil: now.Add(48 * time.Hour),
			},
			{
				SnapshotID:      "snap-locked-2",
				CreatedAt:       now.Add(-5 * time.Hour),
				WORMEnabled:     true,
				WORMLocked:      true,
				WORMLegalHold:   true,
			},
			{
				SnapshotID:  "snap-unlocked-1",
				CreatedAt:   now.Add(-10 * time.Hour),
				WORMEnabled: false,
			},
			{
				SnapshotID:      "snap-expired-worm",
				CreatedAt:       now.Add(-20 * time.Hour),
				WORMEnabled:     true,
				WORMLocked:      false,
				WORMRetainUntil: now.Add(-1 * time.Hour),
			},
			{
				SnapshotID: "snap-tampered-quarantine",
				CreatedAt:  now.Add(-30 * time.Hour),
				Tampered:   true,
			},
		}

		plan := CalculatePrunePlanAt(snapshots, zeroPolicy, now)

		// Assert all WORM-locked snapshots, tampered snapshots, and latest snapshot are in Keep
		keepMap := make(map[string]bool)
		for _, s := range plan.Keep {
			keepMap[s.SnapshotID] = true
		}

		if !keepMap["snap-latest"] {
			t.Errorf("Latest snapshot must be kept even with 0 retention policy")
		}
		if !keepMap["snap-locked-1"] {
			t.Errorf("WORM locked snapshot snap-locked-1 MUST be kept")
		}
		if !keepMap["snap-locked-2"] {
			t.Errorf("LegalHold snapshot snap-locked-2 MUST be kept")
		}
		if !keepMap["snap-tampered-quarantine"] {
			t.Errorf("Tampered snapshot snap-tampered-quarantine MUST be kept")
		}

		// Ensure no locked snapshots are in Delete
		for _, del := range plan.Delete {
			if del.SnapshotID == "snap-locked-1" || del.SnapshotID == "snap-locked-2" || del.SnapshotID == "snap-tampered-quarantine" {
				t.Fatalf("CRITICAL BUG: WORM/Tampered snapshot %s was placed in Delete list!", del.SnapshotID)
			}
		}

		// Non-locked snapshots can be in Delete under zero policy
		if !hasSnapshot(plan.Delete, "snap-unlocked-1") {
			t.Errorf("Expected snap-unlocked-1 to be deleted under 0 retention policy")
		}
		if !hasSnapshot(plan.Delete, "snap-expired-worm") {
			t.Errorf("Expected snap-expired-worm to be deleted under 0 retention policy")
		}
	})

	t.Run("BoundaryTimestamp_RetainUntilExactMatch", func(t *testing.T) {
		mExact := SnapshotManifest{
			WORM: WORMMetadata{
				Enabled:     true,
				RetainUntil: now,
			},
		}
		if mExact.IsWORMLocked(now) {
			t.Errorf("At exact RetainUntil timestamp, IsWORMLocked should be false (expired)")
		}

		mFuture := SnapshotManifest{
			WORM: WORMMetadata{
				Enabled:     true,
				RetainUntil: now.Add(1 * time.Nanosecond),
			},
		}
		if !mFuture.IsWORMLocked(now) {
			t.Errorf("RetainUntil 1ns in future MUST be locked")
		}

		mPast := SnapshotManifest{
			WORM: WORMMetadata{
				Enabled:     true,
				RetainUntil: now.Add(-1 * time.Nanosecond),
			},
		}
		if mPast.IsWORMLocked(now) {
			t.Errorf("RetainUntil 1ns in past MUST be unlocked")
		}
	})

	t.Run("LegalHold_Overrides_Any_Expiry_Even_100_Years_Ago", func(t *testing.T) {
		mCentury := SnapshotManifest{
			WORM: WORMMetadata{
				Enabled:     true,
				RetainUntil: now.Add(-100 * 365 * 24 * time.Hour),
				LegalHold:   true,
			},
		}
		if !mCentury.IsWORMLocked(now) {
			t.Fatalf("LegalHold MUST override RetainUntil even 100 years expired!")
		}

		sum := mCentury.Summary()
		if !sum.IsWORMLocked(now) {
			t.Fatalf("SnapshotSummary with LegalHold MUST return true for IsWORMLocked!")
		}
	})

	t.Run("AllSnapshotsLocked_ZeroDeletions", func(t *testing.T) {
		var snapshots []SnapshotSummary
		for i := 0; i < 50; i++ {
			snapshots = append(snapshots, SnapshotSummary{
				SnapshotID:      fmt.Sprintf("snap-all-locked-%02d", i),
				CreatedAt:       now.Add(-time.Duration(i) * time.Hour),
				WORMEnabled:     true,
				WORMLocked:      true,
				WORMRetainUntil: now.Add(30 * 24 * time.Hour),
			})
		}

		plan := CalculatePrunePlanAt(snapshots, policy, now)
		if len(plan.Delete) != 0 {
			t.Fatalf("Expected 0 deletions when all snapshots are WORM-locked, got %d", len(plan.Delete))
		}
		if len(plan.Keep) != 50 {
			t.Fatalf("Expected all 50 snapshots to be kept, got %d", len(plan.Keep))
		}
	})

	t.Run("SingleSnapshot_AlwaysKept", func(t *testing.T) {
		single := []SnapshotSummary{
			{
				SnapshotID:  "snap-only-one",
				CreatedAt:   now.Add(-100 * time.Hour),
				WORMEnabled: false,
			},
		}
		plan := CalculatePrunePlanAt(single, policy, now)
		if len(plan.Keep) != 1 || len(plan.Delete) != 0 {
			t.Fatalf("Single snapshot must always be kept")
		}
	})
}

// TestAdversarial_DisasterRecovery_IndexWipeAndReindex tests complete local index wipe
// and restoration from Telegram append-only cloud updates.
func TestAdversarial_DisasterRecovery_IndexWipeAndReindex(t *testing.T) {
	key := "disaster-recovery-test-key-32-b!"
	now := time.Now().Truncate(time.Second)

	// Create 3 genuine WORM snapshots
	var manifests []*SnapshotManifest
	for i := 1; i <= 3; i++ {
		m := &SnapshotManifest{
			SnapshotID: fmt.Sprintf("snap-dr-test-%03d", i),
			CreatedAt:  now.Add(-time.Duration(i) * time.Hour),
			NodeName:   "Production-NAS",
			TotalFiles: i * 10,
			TotalBytes: int64(i * 1048576),
			WORM: WORMMetadata{
				Enabled:       true,
				Mode:          WORMModeCompliance,
				LockedAt:      now.Add(-time.Duration(i) * time.Hour),
				RetainUntil:   now.Add(30 * 24 * time.Hour),
				RetentionDays: 30,
			},
			Files: make(map[string]FileEntry),
		}
		m.WORM.ManifestHMAC = m.ComputeHMAC(key)
		manifests = append(manifests, m)
	}

	encManifests := make(map[string][]byte)
	for _, m := range manifests {
		enc, err := m.Encrypt(key)
		if err != nil {
			t.Fatalf("failed to encrypt manifest %s: %v", m.SnapshotID, err)
		}
		encManifests[m.SnapshotID] = enc
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/botreindextok/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var updates []map[string]interface{}
		for idx, m := range manifests {
			updates = append(updates, map[string]interface{}{
				"update_id": int64(idx + 1),
				"message": map[string]interface{}{
					"message_id": int64(100 + idx),
					"caption":    fmt.Sprintf("[TCDRIVE_MANIFEST] %s | Files: %d | Size: %d", m.SnapshotID, m.TotalFiles, m.TotalBytes),
					"document": map[string]interface{}{
						"file_id":   "fid_" + m.SnapshotID,
						"file_name": m.SnapshotID + ".manifest.enc",
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

	mux.HandleFunc("/botreindextok/getFile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fid := r.URL.Query().Get("file_id")
		snapID := strings.TrimPrefix(fid, "fid_")
		resp := map[string]interface{}{
			"ok": true,
			"result": map[string]interface{}{
				"file_id":   fid,
				"file_path": "docs/" + snapID,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/file/botreindextok/", func(w http.ResponseWriter, r *http.Request) {
		snapID := filepath.Base(r.URL.Path)
		if enc, exists := encManifests[snapID]; exists {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(enc)
			return
		}
		http.NotFound(w, r)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	t.Setenv("TELEGRAM_API_URL", server.URL)

	tempDir := t.TempDir()
	snapDir := filepath.Join(tempDir, "reconstructed_snapshots")

	cfg := &config.Config{
		BotTokens:     []string{"reindextok"},
		ChannelID:     "-100111222333",
		EncryptionKey: key,
		StateDir:      snapDir,
	}

	engine, err := NewEngine(cfg, nil)
	if err != nil {
		t.Fatalf("Failed to initialize Engine: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	summary, err := engine.ReindexFromTelegram(ctx)
	if err != nil {
		t.Fatalf("ReindexFromTelegram failed: %v", err)
	}

	if summary.ManifestsRestored != 3 {
		t.Errorf("Expected 3 manifests restored, got %d", summary.ManifestsRestored)
	}

	for _, m := range manifests {
		filePath := filepath.Join(snapDir, m.SnapshotID+".json")
		if _, err := os.Stat(filePath); err != nil {
			t.Errorf("Manifest file %s not found on disk after reindex: %v", m.SnapshotID, err)
		}

		loaded, err := engine.GetSnapshot(m.SnapshotID)
		if err != nil {
			t.Errorf("Failed to read restored snapshot %s: %v", m.SnapshotID, err)
			continue
		}
		if loaded.Tampered {
			t.Errorf("Restored snapshot %s was erroneously flagged as tampered!", m.SnapshotID)
		}
		if !loaded.IsWORMLocked(time.Now()) {
			t.Errorf("Restored snapshot %s should be WORM-locked!", m.SnapshotID)
		}
	}
}

func hasSnapshot(list []SnapshotSummary, id string) bool {
	for _, s := range list {
		if s.SnapshotID == id {
			return true
		}
	}
	return false
}
