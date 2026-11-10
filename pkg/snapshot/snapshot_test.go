package snapshot

import (
	"fmt"
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
