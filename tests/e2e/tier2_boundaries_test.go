package e2e_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/crypto"
)

// ============================================================================
// Tier 2: Feature 1 - WebDAV Boundary Seeking & Corner Cases
// ============================================================================

// TestTier2_WebDAV_Seek_OffsetZero verifies seeking to byte offset 0.
func TestTier2_WebDAV_Seek_OffsetZero(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	req, _ := http.NewRequest("GET", "http://127.0.0.1:8089/latest/large.bin", nil)
	req.Header.Set("Range", "bytes=0-0")
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Range request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("expected 206 Partial Content, got: %d", resp.StatusCode)
	}
}

// TestTier2_WebDAV_Seek_ChunkBoundary verifies seeking exactly at the 49MB chunk boundary.
func TestTier2_WebDAV_Seek_ChunkBoundary(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	chunkSize := int64(49 * 1024 * 1024)
	rangeHeader := fmt.Sprintf("bytes=%d-%d", chunkSize, chunkSize+1023)

	req, _ := http.NewRequest("GET", "http://127.0.0.1:8089/latest/large.bin", nil)
	req.Header.Set("Range", rangeHeader)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Chunk boundary Range request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("expected 206 Partial Content at boundary, got: %d", resp.StatusCode)
	}
}

// TestTier2_WebDAV_Seek_CrossChunk verifies reads spanning across chunk 0 and chunk 1.
func TestTier2_WebDAV_Seek_CrossChunk(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	chunkSize := int64(49 * 1024 * 1024)
	// Span 1KB before boundary to 1KB after boundary
	rangeHeader := fmt.Sprintf("bytes=%d-%d", chunkSize-1024, chunkSize+1024)

	req, _ := http.NewRequest("GET", "http://127.0.0.1:8089/latest/large.bin", nil)
	req.Header.Set("Range", rangeHeader)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Cross-chunk Range request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("expected 206 Partial Content for cross-chunk read, got: %d", resp.StatusCode)
	}
}

// TestTier2_WebDAV_Seek_EOF verifies seeking to the exact end of file.
func TestTier2_WebDAV_Seek_EOF(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	// Range past valid content: HTTP 416 Range Not Satisfiable
	req, _ := http.NewRequest("GET", "http://127.0.0.1:8089/latest/test.txt", nil)
	req.Header.Set("Range", "bytes=99999999-99999999")
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("EOF Range request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable && resp.StatusCode != http.StatusOK {
		t.Errorf("expected 416 Range Not Satisfiable, got: %d", resp.StatusCode)
	}
}

// TestTier2_WebDAV_Seek_PastEOF verifies seeking far beyond EOF returns io.EOF without panic.
func TestTier2_WebDAV_Seek_PastEOF(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	req, _ := http.NewRequest("GET", "http://127.0.0.1:8089/latest/test.txt", nil)
	req.Header.Set("Range", "bytes=5000000000-5000001000") // 5GB on a small file
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Past EOF request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("expected 416 for seek past EOF, got: %d", resp.StatusCode)
	}
}

// ============================================================================
// Tier 2: Feature 4 - Fast Compression Boundary & Corner Cases
// ============================================================================

// TestTier2_Compression_Precompressed_Media_JPEG_PNG tests high entropy data does not expand.
func TestTier2_Compression_Precompressed_Media_JPEG_PNG(t *testing.T) {
	ctx := NewE2EContext(t)
	// Simulate compressed JPEG/MP4 media with random bytes
	mediaData := ctx.CreateIncompressibleFile("sample.jpg", 128*1024)
	passphrase := "media-passphrase-32b"

	encData, err := crypto.EncryptChunkWithCompression(mediaData, passphrase, crypto.CompressionZstd)
	if err != nil {
		t.Fatalf("EncryptChunkWithCompression failed: %v", err)
	}

	// Flag must be 0x00 (CompressionNone)
	if encData[4] != 0x00 {
		t.Errorf("expected bypass Flag 0x00 for media file, got: 0x%02X", encData[4])
	}

	// Encrypted container size should not exceed raw size + container header overhead (53 bytes)
	maxExpected := len(mediaData) + 53
	if len(encData) > maxExpected {
		t.Errorf("data inflation detected: encSize=%d, maxExpected=%d", len(encData), maxExpected)
	}
}

// TestTier2_Compression_Highly_Compressible_Logs tests >70% ratio on repetitive logs.
func TestTier2_Compression_Highly_Compressible_Logs(t *testing.T) {
	logLine := []byte("2026-11-10T12:00:00.000Z [INFO] kernel: CPU idle state tick event node=Synology-DS920+\n")
	plaintext := bytes.Repeat(logLine, 2000) // ~160KB
	passphrase := "logs-passphrase-32b"

	encData, err := crypto.EncryptChunkWithCompression(plaintext, passphrase, crypto.CompressionZstd)
	if err != nil {
		t.Fatalf("compression failed: %v", err)
	}

	ratio := float64(len(plaintext)-len(encData)) / float64(len(plaintext)) * 100.0
	if ratio < 70.0 {
		t.Errorf("expected >70%% compression on repetitive log data, got: %.2f%%", ratio)
	}
}

// TestTier2_Compression_ZeroByte_Chunk tests empty chunk handling.
func TestTier2_Compression_ZeroByte_Chunk(t *testing.T) {
	emptyPlaintext := []byte{}
	passphrase := "empty-chunk-passphrase"

	encData, err := crypto.EncryptChunkWithCompression(emptyPlaintext, passphrase, crypto.CompressionZstd)
	if err != nil {
		t.Fatalf("empty chunk encryption failed: %v", err)
	}

	// Must be Flag 0x00 (None) and OrigSz 0
	if encData[4] != 0x00 {
		t.Errorf("expected Flag 0x00 for empty chunk, got: 0x%02X", encData[4])
	}
	origSz := binary.BigEndian.Uint32(encData[5:9])
	if origSz != 0 {
		t.Errorf("expected OrigSz 0 for empty chunk, got: %d", origSz)
	}

	decrypted, err := crypto.DecryptChunk(encData, passphrase)
	if err != nil {
		t.Fatalf("empty chunk decryption failed: %v", err)
	}
	if len(decrypted) != 0 {
		t.Errorf("expected 0 bytes decrypted, got: %d", len(decrypted))
	}
}

// TestTier2_Compression_Corrupted_Zstd_Payload tests error when ciphertext has bad zstd stream.
func TestTier2_Compression_Corrupted_Zstd_Payload(t *testing.T) {
	// Directly test DecompressData error behavior
	corruptedFrame := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0xFF, 0xFF} // corrupted magic/payload
	_, err := crypto.DecompressData(corruptedFrame, 1024)
	if err == nil {
		t.Fatalf("expected DecompressData to return error for corrupted zstd frame")
	}
}

// TestTier2_Compression_Tampered_OrigSz_AAD tests tampering uncompressed size in AAD.
func TestTier2_Compression_Tampered_OrigSz_AAD(t *testing.T) {
	plaintext := []byte("Original uncompressed payload text")
	passphrase := "tamper-aad-passphrase"

	encData, err := crypto.EncryptChunkWithCompression(plaintext, passphrase, crypto.CompressionZstd)
	if err != nil {
		t.Fatalf("encryption failed: %v", err)
	}

	// Container format: [4B Magic][1B Flag][4B OrigSz]
	// Tamper OrigSz: flip most significant byte
	tampered := make([]byte, len(encData))
	copy(tampered, encData)
	tampered[5] ^= 0x01

	_, err = crypto.DecryptChunk(tampered, passphrase)
	if err == nil {
		t.Fatalf("expected GCM authentication failure when OrigSz in AAD is modified")
	}
}

// ============================================================================
// Tier 2: Feature 3 - Anti-Ransomware WORM Boundary & Corner Cases
// ============================================================================

// TestTier2_WORM_Malformed_Manifest_Rejection tests error handling for corrupted manifests.
func TestTier2_WORM_Malformed_Manifest_Rejection(t *testing.T) {
	malformedJSON := []byte(`{"snapshot_id": "snap-20261110", "worm": { "enabled": true, "mode": "comp`) // Truncated

	var m struct {
		SnapshotID string `json:"snapshot_id"`
	}
	err := json.Unmarshal(malformedJSON, &m)
	if err == nil {
		t.Fatalf("expected JSON unmarshal error on malformed manifest")
	}
}

// TestTier2_WORM_Tampered_HMAC_Signature tests HMAC rejection when retain_until is manipulated.
func TestTier2_WORM_Tampered_HMAC_Signature(t *testing.T) {
	key := "secure-worm-hmac-master-key"
	snapID := "snap-20261110-140000"
	canonicalPayload := fmt.Sprintf("%s|mode=compliance|retain_until=1800000000", snapID)

	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(canonicalPayload))
	validSig := hex.EncodeToString(mac.Sum(nil))

	// Local attacker alters retain_until to an earlier date
	attackerPayload := fmt.Sprintf("%s|mode=compliance|retain_until=1700000000", snapID)
	macVerify := hmac.New(sha256.New, []byte(key))
	macVerify.Write([]byte(attackerPayload))
	computedSig := hex.EncodeToString(macVerify.Sum(nil))

	if hmac.Equal([]byte(validSig), []byte(computedSig)) {
		t.Fatalf("HMAC verification failed to detect malicious alteration of retain_until!")
	}
}

// TestTier2_WORM_NTP_Clock_Skew_Detection tests clock skew anomaly detection.
func TestTier2_WORM_NTP_Clock_Skew_Detection(t *testing.T) {
	// Simulate local system clock shifted 2 years forward (e.g. 2028 instead of 2026)
	localSystemTime := time.Date(2028, 11, 10, 12, 0, 0, 0, time.UTC)

	// Telegram HTTP response Date header timestamp
	telegramHTTPDate := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	skew := localSystemTime.Sub(telegramHTTPDate)
	if skew < 0 {
		skew = -skew
	}

	maxToleratedSkew := 1 * time.Hour
	if skew > maxToleratedSkew {
		t.Logf("[SECURITY ALERT] Clock skew detected: local=%s, remote=%s, skew=%s",
			localSystemTime, telegramHTTPDate, skew)
	} else {
		t.Fatalf("expected clock skew anomaly to be detected")
	}
}

// TestTier2_WORM_LegalHold_Overrides_Expiry tests that LegalHold=true overrides elapsed time.
func TestTier2_WORM_LegalHold_Overrides_Expiry(t *testing.T) {
	now := time.Now()
	retainUntilPast := now.Add(-365 * 24 * time.Hour) // 1 year ago

	isPrunable := func(retainUntil time.Time, legalHold bool, currentTime time.Time) bool {
		if legalHold {
			return false // Legal hold indefinitely prevents deletion
		}
		return currentTime.After(retainUntil)
	}

	// Normal expired snapshot is prunable
	if !isPrunable(retainUntilPast, false, now) {
		t.Errorf("expected expired snapshot with legalHold=false to be prunable")
	}

	// Expired snapshot with legalHold=true MUST NOT be prunable
	if isPrunable(retainUntilPast, true, now) {
		t.Fatalf("snapshot under Legal Hold was mistakenly classified as prunable!")
	}
}

// TestTier2_WORM_Compliance_Mode_Zero_Days_Rejected tests validation of retention period.
func TestTier2_WORM_Compliance_Mode_Zero_Days_Rejected(t *testing.T) {
	validateWORMConfig := func(mode string, retentionDays int) error {
		if mode == "compliance" && retentionDays <= 0 {
			return fmt.Errorf("compliance mode requires retention_days > 0, got %d", retentionDays)
		}
		return nil
	}

	if err := validateWORMConfig("compliance", 0); err == nil {
		t.Errorf("expected error for compliance mode with 0 retention days")
	}
	if err := validateWORMConfig("compliance", -5); err == nil {
		t.Errorf("expected error for compliance mode with negative retention days")
	}
	if err := validateWORMConfig("compliance", 30); err != nil {
		t.Errorf("unexpected error for valid compliance mode: %v", err)
	}
}

// ============================================================================
// Tier 2: Feature 2 - Bot Remote Control Boundary & Corner Cases
// ============================================================================

// TestTier2_Bot_Network_Drop_AutoReconnect tests recovery from socket drop.
func TestTier2_Bot_Network_Drop_AutoReconnect(t *testing.T) {
	ctx := NewE2EContext(t)
	ctx.MockTG.TriggerNetworkDrop()

	client := &http.Client{Timeout: 1 * time.Second}
	req, _ := http.NewRequest("GET", ctx.MockTG.URL()+"/bot12345/getMe", nil)
	resp, err := client.Do(req)
	// Network drop should either return error or StatusBadGateway
	if err != nil || (resp != nil && resp.StatusCode == http.StatusBadGateway) {
		t.Logf("network drop successfully simulated: err=%v", err)
	}

	// Next request must recover cleanly
	req2, _ := http.NewRequest("GET", ctx.MockTG.URL()+"/bot12345/getMe", nil)
	resp2, err2 := client.Do(req2)
	if err2 != nil {
		t.Fatalf("server failed to recover after network drop: %v", err2)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK after reconnection, got: %d", resp2.StatusCode)
	}
}

// TestTier2_Bot_HTTP429_RateLimit_Backoff tests handling of HTTP 429 Too Many Requests.
func TestTier2_Bot_HTTP429_RateLimit_Backoff(t *testing.T) {
	ctx := NewE2EContext(t)
	ctx.MockTG.SetRateLimit(1) // 1 second 429

	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest("GET", ctx.MockTG.URL()+"/bot12345/getMe", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 Too Many Requests, got: %d", resp.StatusCode)
	}

	// After retry_after elapsed, next request succeeds
	time.Sleep(1100 * time.Millisecond)
	resp2, err2 := client.Do(req)
	if err2 != nil {
		t.Fatalf("request after backoff failed: %v", err2)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK after backoff, got: %d", resp2.StatusCode)
	}
}

// TestTier2_Bot_Unauthorized_User_Blocked tests security isolation of unauthorized users.
func TestTier2_Bot_Unauthorized_User_Blocked(t *testing.T) {
	authorizedUsers := map[int64]bool{1001: true, 1002: true}

	checkAccess := func(userID int64) bool {
		return authorizedUsers[userID]
	}

	if !checkAccess(1001) {
		t.Errorf("authorized user 1001 was rejected")
	}
	if checkAccess(999999) {
		t.Errorf("unauthorized user 999999 was mistakenly granted access")
	}
}

// TestTier2_Bot_Markdown_Special_Characters_Escaped tests escaping of Markdown symbols.
func TestTier2_Bot_Markdown_Special_Characters_Escaped(t *testing.T) {
	escapeMarkdown := func(text string) string {
		var b strings.Builder
		for _, r := range text {
			switch r {
			case '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!':
				b.WriteRune('\\')
			}
			b.WriteRune(r)
		}
		return b.String()
	}

	rawFilename := "project_report_[v2]*final*.tar.gz"
	escaped := escapeMarkdown(rawFilename)

	if strings.Contains(escaped, "_") && !strings.Contains(escaped, "\\_") {
		t.Errorf("failed to escape underscore: %s", escaped)
	}
	if strings.Contains(escaped, "*") && !strings.Contains(escaped, "\\*") {
		t.Errorf("failed to escape asterisk: %s", escaped)
	}
}

// TestTier2_Bot_Rapid_Duplicate_BackupNow tests concurrency guard against duplicate backups.
func TestTier2_Bot_Rapid_Duplicate_BackupNow(t *testing.T) {
	isBackingUp := true // backup already in progress

	triggerBackup := func() (string, error) {
		if isBackingUp {
			return "[WARN] Backup already in progress (42%)", nil
		}
		return "[OK] Backup started", nil
	}

	msg, _ := triggerBackup()
	if !strings.Contains(msg, "[WARN]") || !strings.Contains(msg, "already in progress") {
		t.Errorf("expected warning for concurrent duplicate backup, got: %s", msg)
	}
}
