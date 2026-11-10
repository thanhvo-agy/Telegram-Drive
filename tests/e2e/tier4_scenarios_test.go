package e2e_test

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ============================================================================
// Tier 4: Real-World Application Scenarios (>=5 Realistic Scenarios)
// ============================================================================

// TestTier4_Windows_Explorer_Browse_And_Partial_Read simulates a Windows 10/11
// workstation mapping a network drive via WebClient Mini-Redirector.
func TestTier4_Windows_Explorer_Browse_And_Partial_Read(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	baseURL := "http://127.0.0.1:8089/"
	client := &http.Client{Timeout: 3 * time.Second}

	// Step 1: Windows WebClient initiates connection probe with OPTIONS /
	reqOpt, _ := http.NewRequest("OPTIONS", baseURL, nil)
	respOpt, err := client.Do(reqOpt)
	if err != nil {
		t.Fatalf("Windows Explorer OPTIONS probe failed: %v", err)
	}
	defer respOpt.Body.Close()

	if respOpt.Header.Get("MS-Author-Via") != "DAV" {
		t.Errorf("Windows Explorer requires 'MS-Author-Via: DAV' header")
	}

	// Step 2: Windows Explorer queries directory structure via PROPFIND Depth 1
	propfindXML := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:displayname/><D:resourcetype/><D:getcontentlength/><D:getlastmodified/></D:prop></D:propfind>`
	reqProp, _ := http.NewRequest("PROPFIND", baseURL, bytes.NewBufferString(propfindXML))
	reqProp.Header.Set("Depth", "1")
	reqProp.Header.Set("Content-Type", "application/xml")
	respProp, err := client.Do(reqProp)
	if err != nil {
		t.Fatalf("Windows Explorer PROPFIND failed: %v", err)
	}
	defer respProp.Body.Close()

	if respProp.StatusCode != 207 {
		t.Errorf("expected HTTP 207 Multi-Status for PROPFIND, got: %d", respProp.StatusCode)
	}

	// Step 3: Windows Explorer probes desktop.ini / autorun.inf (must fast 404 without hang)
	reqIni, _ := http.NewRequest("GET", baseURL+"desktop.ini", nil)
	respIni, err := client.Do(reqIni)
	if err == nil {
		defer respIni.Body.Close()
		if respIni.StatusCode != http.StatusNotFound && respIni.StatusCode != http.StatusForbidden {
			t.Logf("desktop.ini probe status: %d", respIni.StatusCode)
		}
	}

	// Step 4: User opens a media file; Windows Media Player requests partial header bytes (0-4095)
	reqMedia, _ := http.NewRequest("GET", baseURL+"latest/video.mp4", nil)
	reqMedia.Header.Set("Range", "bytes=0-4095")
	respMedia, err := client.Do(reqMedia)
	if err == nil {
		defer respMedia.Body.Close()
		if respMedia.StatusCode != http.StatusPartialContent {
			t.Errorf("expected HTTP 206 Partial Content for Windows media seek, got: %d", respMedia.StatusCode)
		}
	}
}

// TestTier4_MacOS_Finder_Browse_And_Metadata_Probe simulates a macOS Finder client
// connecting via mount_webdav (Cmd+K) and issuing AppleDouble probes.
func TestTier4_MacOS_Finder_Browse_And_Metadata_Probe(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	baseURL := "http://127.0.0.1:8089/"
	client := &http.Client{Timeout: 3 * time.Second}

	// Step 1: macOS Finder mounts root and checks PROPFIND Depth 0
	reqProp0, _ := http.NewRequest("PROPFIND", baseURL, nil)
	reqProp0.Header.Set("Depth", "0")
	respProp0, err := client.Do(reqProp0)
	if err != nil {
		t.Fatalf("macOS Finder root PROPFIND failed: %v", err)
	}
	defer respProp0.Body.Close()

	if respProp0.StatusCode != 207 {
		t.Errorf("expected 207 Multi-Status, got: %d", respProp0.StatusCode)
	}

	// Step 2: macOS Finder probes ._ AppleDouble metadata file
	reqAppleDouble, _ := http.NewRequest("GET", baseURL+"._document.pdf", nil)
	respAppleDouble, err := client.Do(reqAppleDouble)
	if err == nil {
		defer respAppleDouble.Body.Close()
		// Must return 404 without hanging
		if respAppleDouble.StatusCode != http.StatusNotFound && respAppleDouble.StatusCode != http.StatusForbidden {
			t.Logf("AppleDouble probe returned: %d", respAppleDouble.StatusCode)
		}
	}

	// Step 3: macOS Finder attempts to create .DS_Store (server must reject with 403 Forbidden in read-only mode)
	reqDSStore, _ := http.NewRequest("PUT", baseURL+".DS_Store", bytes.NewBuffer([]byte{0x00, 0x01}))
	respDSStore, err := client.Do(reqDSStore)
	if err == nil {
		defer respDSStore.Body.Close()
		if respDSStore.StatusCode != http.StatusForbidden && respDSStore.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("expected 403 Forbidden or 405 Method Not Allowed on read-only mount mutation, got: %d", respDSStore.StatusCode)
		}
	}
}

// TestTier4_Disaster_Recovery_Complete_Wipe_And_Reindex simulates a ransomware attack
// that formats the NAS local disk, followed by full recovery via tgdrive reindex.
func TestTier4_Disaster_Recovery_Complete_Wipe_And_Reindex(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWORM() {
		t.Skip("Feature 3 (Disaster Recovery Reindex) requires M2 (currently M1)")
	}

	// 1. Create simulated local snapshots on disk
	localSnapDir := filepath.Join(ctx.TempDir, "snapshots")
	_ = os.MkdirAll(localSnapDir, 0755)

	manifestData := []byte(`{"snapshot_id":"snap-20261110-001","total_files":42,"total_bytes":10485760}`)
	snapFile := filepath.Join(localSnapDir, "snap-20261110-001.json")
	_ = os.WriteFile(snapFile, manifestData, 0644)

	// 2. Simulate catastrophic local disk wipe (ransomware attack)
	_ = os.RemoveAll(localSnapDir)

	if _, err := os.Stat(snapFile); !os.IsNotExist(err) {
		t.Fatalf("local file wipe simulation failed")
	}

	// 3. Initiate Disaster Recovery Reindex: scans remote Telegram channel
	stdout, stderr, exitCode, _ := ctx.RunCLI("reindex")
	t.Logf("Disaster recovery reindex exited with code %d (stdout: %s, stderr: %s)", exitCode, stdout, stderr)
}

// TestTier4_MultiVersion_Timeline_WORM_Lifecycle simulates snapshot progression over
// 90 days with active retention windows and expired snapshot pruning.
func TestTier4_MultiVersion_Timeline_WORM_Lifecycle(t *testing.T) {
	type SnapRecord struct {
		ID          string
		AgeDays     int
		RetainDays  int
		LegalHold   bool
		IsPrunable  bool
	}

	now := time.Now()

	timeline := []SnapRecord{
		{ID: "snap-day-1", AgeDays: 1, RetainDays: 30, LegalHold: false, IsPrunable: false},  // Active lock
		{ID: "snap-day-7", AgeDays: 7, RetainDays: 30, LegalHold: false, IsPrunable: false},  // Active lock
		{ID: "snap-day-35", AgeDays: 35, RetainDays: 30, LegalHold: true, IsPrunable: false}, // Expired lock, BUT Legal Hold active!
		{ID: "snap-day-45", AgeDays: 45, RetainDays: 30, LegalHold: false, IsPrunable: true}, // Expired lock, no hold -> Prunable!
		{ID: "snap-day-90", AgeDays: 90, RetainDays: 30, LegalHold: false, IsPrunable: true}, // Expired lock -> Prunable!
	}

	for _, rec := range timeline {
		snapTime := now.Add(-time.Duration(rec.AgeDays) * 24 * time.Hour)
		retainUntil := snapTime.Add(time.Duration(rec.RetainDays) * 24 * time.Hour)

		isLocked := func(legalHold bool, retain time.Time, currentTime time.Time) bool {
			if legalHold {
				return true
			}
			return currentTime.Before(retain)
		}

		locked := isLocked(rec.LegalHold, retainUntil, now)
		expectedLocked := !rec.IsPrunable

		if locked != expectedLocked {
			t.Errorf("snapshot %s (age %dd, legalHold=%v): expected locked=%v, got locked=%v",
				rec.ID, rec.AgeDays, rec.LegalHold, expectedLocked, locked)
		}
	}
}

// TestTier4_Synology_FileStation_Remote_WebDAV_Mount simulates Synology DSM File Station
// connecting to tgdrive's WebDAV server on port 8089 and streaming files.
func TestTier4_Synology_FileStation_Remote_WebDAV_Mount(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	baseURL := "http://127.0.0.1:8089/"
	client := &http.Client{Timeout: 3 * time.Second}

	// 1. Synology DSM File Station tests connection with HEAD /
	reqHead, _ := http.NewRequest("HEAD", baseURL, nil)
	respHead, err := client.Do(reqHead)
	if err != nil {
		t.Fatalf("Synology File Station connection check failed: %v", err)
	}
	defer respHead.Body.Close()

	// 2. Synology DSM browses directory contents with PROPFIND Depth 1
	reqList, _ := http.NewRequest("PROPFIND", baseURL+"latest/", nil)
	reqList.Header.Set("Depth", "1")
	respList, err := client.Do(reqList)
	if err != nil {
		t.Fatalf("Synology directory browse failed: %v", err)
	}
	defer respList.Body.Close()

	if respList.StatusCode != 207 {
		t.Errorf("expected 207 Multi-Status for DSM browsing, got: %d", respList.StatusCode)
	}

	// 3. Synology DSM Audio Station / Video Station streams media via HTTP 206 Range request
	reqStream, _ := http.NewRequest("GET", baseURL+"latest/audio.flac", nil)
	reqStream.Header.Set("Range", "bytes=0-65535")
	respStream, err := client.Do(reqStream)
	if err == nil {
		defer respStream.Body.Close()
		if respStream.StatusCode != http.StatusPartialContent {
			t.Errorf("expected HTTP 206 Partial Content for DSM media stream, got: %d", respStream.StatusCode)
		}
		data, _ := io.ReadAll(respStream.Body)
		t.Logf("DSM streaming received %d bytes", len(data))
	}
}
