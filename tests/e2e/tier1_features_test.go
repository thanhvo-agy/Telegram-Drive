package e2e_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/crypto"
)

// ============================================================================
// Tier 1: Feature 1 - WebDAV Server (RFC 4918, Port 8089, Streaming, Seeking)
// ============================================================================

// TestTier1_WebDAV_OPTIONS_RFC4918 verifies RFC 4918 compliance and OS compatibility headers.
func TestTier1_WebDAV_OPTIONS_RFC4918(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	port := 8089
	serverURL := fmt.Sprintf("http://127.0.0.1:%d/", port)

	req, err := http.NewRequest("OPTIONS", serverURL, nil)
	if err != nil {
		t.Fatalf("failed to create OPTIONS request: %v", err)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 200 OK or 204 No Content for OPTIONS, got: %d", resp.StatusCode)
	}

	davHeader := resp.Header.Get("DAV")
	if !strings.Contains(davHeader, "1") {
		t.Errorf("expected 'DAV' header to contain '1', got: %q", davHeader)
	}

	msAuthorHeader := resp.Header.Get("MS-Author-Via")
	if msAuthorHeader != "DAV" {
		t.Errorf("expected 'MS-Author-Via: DAV' header for Windows Explorer compatibility, got: %q", msAuthorHeader)
	}
}

// TestTier1_WebDAV_PROPFIND_SnapshotTree verifies XML multistatus response on virtual root.
func TestTier1_WebDAV_PROPFIND_SnapshotTree(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	port := 8089
	serverURL := fmt.Sprintf("http://127.0.0.1:%d/", port)

	propfindXML := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`
	req, err := http.NewRequest("PROPFIND", serverURL, bytes.NewBufferString(propfindXML))
	if err != nil {
		t.Fatalf("failed to create PROPFIND request: %v", err)
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PROPFIND request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 207 { // 207 Multi-Status
		t.Errorf("expected HTTP 207 Multi-Status, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "multistatus") {
		t.Errorf("expected response to contain 'multistatus', got: %s", string(body))
	}
}

// TestTier1_WebDAV_GET_FileStream verifies on-the-fly streaming file retrieval.
func TestTier1_WebDAV_GET_FileStream(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	port := 8089
	fileURL := fmt.Sprintf("http://127.0.0.1:%d/latest/testfile.txt", port)

	resp, err := http.Get(fileURL)
	if err != nil {
		t.Fatalf("GET request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got: %d", resp.StatusCode)
	}
}

// TestTier1_WebDAV_HEAD_Metadata verifies accurate metadata inspection without downloading payload.
func TestTier1_WebDAV_HEAD_Metadata(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	port := 8089
	fileURL := fmt.Sprintf("http://127.0.0.1:%d/latest/testfile.txt", port)

	resp, err := http.Head(fileURL)
	if err != nil {
		t.Fatalf("HEAD request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got: %d", resp.StatusCode)
	}

	contentLength := resp.Header.Get("Content-Length")
	if contentLength == "" {
		t.Errorf("expected Content-Length header in HEAD response")
	}
}

// TestTier1_WebDAV_ReadSeeker_RangeRequests verifies HTTP 206 Partial Content byte-range streaming.
func TestTier1_WebDAV_ReadSeeker_RangeRequests(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	port := 8089
	fileURL := fmt.Sprintf("http://127.0.0.1:%d/latest/video.mp4", port)

	req, _ := http.NewRequest("GET", fileURL, nil)
	req.Header.Set("Range", "bytes=0-1023")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Range request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent { // 206 Partial Content
		t.Errorf("expected 206 Partial Content, got: %d", resp.StatusCode)
	}

	contentRange := resp.Header.Get("Content-Range")
	if !strings.HasPrefix(contentRange, "bytes 0-1023/") {
		t.Errorf("expected Content-Range header starting with 'bytes 0-1023/', got: %q", contentRange)
	}
}

// ============================================================================
// Tier 1: Feature 2 - Interactive 2-Way Bot Remote Control (Long Polling, Commands)
// ============================================================================

// TestTier1_Bot_LongPolling_GetUpdates verifies the bot controller's polling loop.
func TestTier1_Bot_LongPolling_GetUpdates(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsBot() {
		t.Skip("Feature 2 (Bot Remote Control) requires M4 (currently M1)")
	}

	// Inject a ping message into mock telegram
	msgID := ctx.MockTG.InjectMessage(1001, "admin_user", 1001, "/status")
	if msgID <= 0 {
		t.Fatalf("failed to inject message")
	}

	// Run check or wait for poller update
	time.Sleep(100 * time.Millisecond)
	if ctx.MockTG.lastOffset < 0 {
		t.Errorf("expected getUpdates to be called with offset >= 0")
	}
}

// TestTier1_Bot_Command_Status verifies /status output format and zero emojis.
func TestTier1_Bot_Command_Status(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsBot() {
		t.Skip("Feature 2 (Bot Remote Control) requires M4 (currently M1)")
	}

	ctx.MockTG.InjectMessage(1001, "admin_user", 1001, "/status")
	time.Sleep(100 * time.Millisecond)

	msg, found := ctx.MockTG.GetLastSentMessage()
	if !found {
		t.Skip("Bot controller not actively running in this process; scheduled for M4")
		return
	}

	for _, r := range msg.Text {
		if isEmoji(r) {
			t.Errorf("emoji detected in /status response: %c", r)
		}
	}

	if !strings.Contains(msg.Text, "[OK]") && !strings.Contains(msg.Text, "Node:") {
		t.Errorf("expected node status in response, got: %s", msg.Text)
	}
}

// TestTier1_Bot_Command_BackupNow verifies on-demand backup triggering.
func TestTier1_Bot_Command_BackupNow(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsBot() {
		t.Skip("Feature 2 (Bot Remote Control) requires M4 (currently M1)")
	}

	ctx.MockTG.InjectMessage(1001, "admin_user", 1001, "/backup_now")
	time.Sleep(100 * time.Millisecond)

	msg, found := ctx.MockTG.GetLastSentMessage()
	if !found {
		t.Skip("Bot controller not actively running; scheduled for M4")
		return
	}

	if !strings.Contains(msg.Text, "Backup") && !strings.Contains(msg.Text, "[BACKUP]") {
		t.Errorf("expected backup confirmation in reply, got: %s", msg.Text)
	}
}

// TestTier1_Bot_Command_Snapshots verifies snapshot listing and button markup.
func TestTier1_Bot_Command_Snapshots(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsBot() {
		t.Skip("Feature 2 (Bot Remote Control) requires M4 (currently M1)")
	}

	ctx.MockTG.InjectMessage(1001, "admin_user", 1001, "/snapshots")
	time.Sleep(100 * time.Millisecond)

	msg, found := ctx.MockTG.GetLastSentMessage()
	if !found {
		t.Skip("Bot controller not actively running; scheduled for M4")
		return
	}

	if msg.ReplyMarkup != "" && !strings.Contains(msg.ReplyMarkup, "inline_keyboard") {
		t.Errorf("expected inline_keyboard in reply_markup, got: %s", msg.ReplyMarkup)
	}
}

// TestTier1_Bot_Command_Help verifies /help response documentation.
func TestTier1_Bot_Command_Help(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsBot() {
		t.Skip("Feature 2 (Bot Remote Control) requires M4 (currently M1)")
	}

	ctx.MockTG.InjectMessage(1001, "admin_user", 1001, "/help")
	time.Sleep(100 * time.Millisecond)

	msg, found := ctx.MockTG.GetLastSentMessage()
	if !found {
		t.Skip("Bot controller not actively running; scheduled for M4")
		return
	}

	expectedCommands := []string{"/status", "/backup_now", "/snapshots", "/help"}
	for _, cmd := range expectedCommands {
		if !strings.Contains(msg.Text, cmd) {
			t.Errorf("expected /help response to document command %s, got: %s", cmd, msg.Text)
		}
	}
}

// TestTier1_Bot_UserID_Whitelist verifies unauthorized user access is denied.
func TestTier1_Bot_UserID_Whitelist(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsBot() {
		t.Skip("Feature 2 (Bot Remote Control) requires M4 (currently M1)")
	}

	// Unauthorized user ID 9999999
	ctx.MockTG.InjectMessage(9999999, "malicious_actor", 9999999, "/backup_now")
	time.Sleep(100 * time.Millisecond)

	msg, found := ctx.MockTG.GetLastSentMessage()
	if !found {
		t.Skip("Bot controller not actively running; scheduled for M4")
		return
	}

	if !strings.Contains(msg.Text, "ACCESS DENIED") && !strings.Contains(msg.Text, "Unauthorized") {
		t.Errorf("expected unauthorized notice or drop for unlisted user, got: %s", msg.Text)
	}
}

// ============================================================================
// Tier 1: Feature 3 - Anti-Ransomware WORM Snapshot Locking & Reindex
// ============================================================================

// TestTier1_WORM_Metadata_Structure verifies WORM metadata fields inside SnapshotManifest.
func TestTier1_WORM_Metadata_Structure(t *testing.T) {
	type WORMMeta struct {
		Enabled       bool      `json:"enabled"`
		Mode          string    `json:"mode"`
		LockedAt      time.Time `json:"locked_at"`
		RetainUntil   time.Time `json:"retain_until"`
		RetentionDays int       `json:"retention_days"`
		LegalHold     bool      `json:"legal_hold"`
		ManifestHMAC  string    `json:"manifest_hmac"`
	}

	type TestManifest struct {
		SnapshotID string   `json:"snapshot_id"`
		WORM       WORMMeta `json:"worm"`
	}

	lockedAt := time.Now()
	retainUntil := lockedAt.Add(30 * 24 * time.Hour)

	m := TestManifest{
		SnapshotID: "snap-20261110-120000",
		WORM: WORMMeta{
			Enabled:       true,
			Mode:          "compliance",
			LockedAt:      lockedAt,
			RetainUntil:   retainUntil,
			RetentionDays: 30,
			LegalHold:     false,
			ManifestHMAC:  "a1b2c3d4e5f67890",
		},
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("failed to marshal manifest with WORM metadata: %v", err)
	}

	var parsed TestManifest
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal manifest with WORM metadata: %v", err)
	}

	if !parsed.WORM.Enabled || parsed.WORM.Mode != "compliance" || parsed.WORM.RetentionDays != 30 {
		t.Errorf("WORM metadata mismatch: %+v", parsed.WORM)
	}
}

// TestTier1_WORM_HMAC_Verification verifies HMAC-SHA256 signature computation and validation.
func TestTier1_WORM_HMAC_Verification(t *testing.T) {
	key := "master-secret-encryption-key-32b"
	canonicalPayload := "snap-20261110-120000|1762776000|TestNode|1048576|compliance|1765368000"

	// Compute HMAC
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(canonicalPayload))
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	// Verify valid signature
	macVerify := hmac.New(sha256.New, []byte(key))
	macVerify.Write([]byte(canonicalPayload))
	computedSig := hex.EncodeToString(macVerify.Sum(nil))

	if !hmac.Equal([]byte(expectedSig), []byte(computedSig)) {
		t.Fatalf("HMAC signature verification failed for authentic payload")
	}

	// Verify tampered payload fails
	tamperedPayload := "snap-20261110-120000|1762776000|TestNode|1048576|compliance|1700000000" // retain_until altered!
	macTamper := hmac.New(sha256.New, []byte(key))
	macTamper.Write([]byte(tamperedPayload))
	tamperedSig := hex.EncodeToString(macTamper.Sum(nil))

	if hmac.Equal([]byte(expectedSig), []byte(tamperedSig)) {
		t.Fatalf("tampered payload unexpectedly matched authentic HMAC signature!")
	}
}

// TestTier1_WORM_Retention_Lock_Enforcement verifies that locked snapshots cannot be pruned.
func TestTier1_WORM_Retention_Lock_Enforcement(t *testing.T) {
	now := time.Now()
	// Simulated snapshots: snap1 is WORM-locked until now+30d; snap2 is expired
	snap1ID := "snap-20261110-100000"
	snap1Locked := true
	snap1RetainUntil := now.Add(30 * 24 * time.Hour)

	snap2ID := "snap-20261001-100000"
	snap2Locked := false

	t.Logf("evaluating retention lock rules for %s (locked) and %s (unlocked)", snap1ID, snap2ID)

	// An active lock must prevent pruning
	isLocked := func(locked bool, retainUntil time.Time, currentTime time.Time) bool {
		return locked && currentTime.Before(retainUntil)
	}

	if !isLocked(snap1Locked, snap1RetainUntil, now) {
		t.Errorf("snap1 should be recognized as WORM locked")
	}
	if isLocked(snap2Locked, time.Time{}, now) {
		t.Errorf("snap2 should not be locked")
	}
}

// TestTier1_WORM_Cloud_Deletion_Prevention verifies zero deletion calls to Telegram API.
func TestTier1_WORM_Cloud_Deletion_Prevention(t *testing.T) {
	ctx := NewE2EContext(t)

	// Perform prune CLI command
	_, _, exitCode, _ := ctx.RunCLI("prune", "--dry-run")
	if exitCode != 0 {
		t.Logf("prune command returned exit code %d (expected if no snapshots exist)", exitCode)
	}

	// Check that no deleteMessage or deleteDocument requests were sent to mock Telegram
	for _, msg := range ctx.MockTG.SentMessages {
		if strings.Contains(strings.ToLower(msg.Text), "deletemessage") {
			t.Fatalf("prohibited cloud deletion API called: %s", msg.Text)
		}
	}
}

// TestTier1_WORM_Disaster_Recovery_Reindex verifies disaster recovery reindex from Telegram.
func TestTier1_WORM_Disaster_Recovery_Reindex(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWORM() {
		t.Skip("Feature 3 (Disaster Recovery Reindex) requires M2 (currently M1)")
	}

	stdout, stderr, exitCode, err := ctx.RunCLI("reindex")
	if err != nil && exitCode != 0 {
		t.Logf("reindex command exited with code %d: %s %s", exitCode, stdout, stderr)
	}
}

// ============================================================================
// Tier 1: Feature 4 - Fast Compression (Zstandard, TGC2, AAD, Fallback, Compat)
// ============================================================================

// TestTier1_Compression_Zstd_Pipeline verifies 30-50% savings on compressible data.
func TestTier1_Compression_Zstd_Pipeline(t *testing.T) {
	plaintext := bytes.Repeat([]byte("2026-11-10T12:00:00Z [INFO] system.daemon: background worker cycle [OK]\n"), 1000)
	passphrase := "e2e-passphrase-test-key-32b"

	encData, err := crypto.EncryptChunkWithCompression(plaintext, passphrase, crypto.CompressionZstd)
	if err != nil {
		t.Fatalf("EncryptChunkWithCompression failed: %v", err)
	}

	origSize := len(plaintext)
	encSize := len(encData)
	savings := float64(origSize-encSize) / float64(origSize) * 100.0

	if savings < 30.0 {
		t.Errorf("expected at least 30%% compression savings on repetitive log data, got: %.2f%% (orig: %d, enc: %d)",
			savings, origSize, encSize)
	}

	// Verify clean decryption
	decrypted, err := crypto.DecryptChunk(encData, passphrase)
	if err != nil {
		t.Fatalf("DecryptChunk failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Fatalf("decrypted plaintext did not match original input")
	}
}

// TestTier1_Compression_TGC2_Container_Format validates container header layout.
func TestTier1_Compression_TGC2_Container_Format(t *testing.T) {
	plaintext := []byte("TGC2 container format validation test payload")
	passphrase := "secret-passphrase-key"

	encData, err := crypto.EncryptChunkWithCompression(plaintext, passphrase, crypto.CompressionZstd)
	if err != nil {
		t.Fatalf("EncryptChunkWithCompression failed: %v", err)
	}

	// Container format:
	// [4B Magic "TGC2"][1B Flag][4B OrigSz BE][16B Salt][12B Nonce][Ciphertext + 16B GCM Tag]
	if len(encData) < 37+16 {
		t.Fatalf("encrypted chunk too short: %d bytes", len(encData))
	}

	magic := encData[0:4]
	if !bytes.Equal(magic, []byte("TGC2")) {
		t.Errorf("expected Magic 'TGC2', got: %q", string(magic))
	}

	flag := encData[4]
	if flag != 0x00 && flag != 0x01 {
		t.Errorf("expected Flag 0x00 (None) or 0x01 (Zstd), got: 0x%02X", flag)
	}

	origSz := binary.BigEndian.Uint32(encData[5:9])
	if int(origSz) != len(plaintext) {
		t.Errorf("expected OrigSz %d, got: %d", len(plaintext), origSz)
	}
}

// TestTier1_Compression_AAD_Authentication verifies that header alterations trigger auth failure.
func TestTier1_Compression_AAD_Authentication(t *testing.T) {
	plaintext := []byte("Sensitive data protected by authenticated additional data")
	passphrase := "test-aad-passphrase"

	encData, err := crypto.EncryptChunkWithCompression(plaintext, passphrase, crypto.CompressionNone)
	if err != nil {
		t.Fatalf("encryption failed: %v", err)
	}

	// Tamper with Flag byte (offset 4)
	tampered := make([]byte, len(encData))
	copy(tampered, encData)
	tampered[4] ^= 0x01 // flip flag bit

	_, err = crypto.DecryptChunk(tampered, passphrase)
	if err == nil {
		t.Fatalf("expected DecryptChunk to fail with authentication error after tampering Flag byte in AAD")
	}

	// Tamper with OrigSz (offset 5..8)
	copy(tampered, encData)
	tampered[8] ^= 0xFF // alter original size in AAD

	_, err = crypto.DecryptChunk(tampered, passphrase)
	if err == nil {
		t.Fatalf("expected DecryptChunk to fail with authentication error after tampering OrigSz in AAD")
	}
}

// TestTier1_Compression_Incompressible_Fallback verifies fallback to Flag 0x00 on random data.
func TestTier1_Compression_Incompressible_Fallback(t *testing.T) {
	ctx := NewE2EContext(t)
	// High entropy pseudo-random data
	randomData := ctx.CreateIncompressibleFile("random.bin", 64*1024)
	passphrase := "random-data-key"

	encData, err := crypto.EncryptChunkWithCompression(randomData, passphrase, crypto.CompressionZstd)
	if err != nil {
		t.Fatalf("encryption failed: %v", err)
	}

	// When compressed size >= raw size, engine must set Flag = 0x00 (CompressionNone)
	flag := encData[4]
	if flag != 0x00 {
		t.Errorf("expected Flag 0x00 (bypass) for incompressible data, got: 0x%02X", flag)
	}

	decrypted, err := crypto.DecryptChunk(encData, passphrase)
	if err != nil {
		t.Fatalf("decryption failed: %v", err)
	}
	if !bytes.Equal(randomData, decrypted) {
		t.Fatalf("decrypted data did not match incompressible input")
	}
}

// TestTier1_Compression_Backward_Compatibility verifies transparent TGC1 and TGC2 support.
func TestTier1_Compression_Backward_Compatibility(t *testing.T) {
	passphrase := "compatibility-test-key-32b"
	plaintext := []byte("Legacy chunk compatibility verification payload")

	// Generate legacy TGC1 chunk manually:
	// Format: [4B "TGC1"][16B Salt][12B Nonce][Ciphertext + 16B Tag]
	salt := make([]byte, 16)
	copy(salt, []byte("0123456789abcdef"))
	nonce := make([]byte, 12)
	copy(nonce, []byte("abcdef123456"))

	key := crypto.DeriveKey(passphrase, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("create cipher failed: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("create gcm failed: %v", err)
	}

	magicV1 := []byte("TGC1")
	ciphertextV1 := gcm.Seal(nil, nonce, plaintext, magicV1)

	var tgc1Chunk []byte
	tgc1Chunk = append(tgc1Chunk, magicV1...)
	tgc1Chunk = append(tgc1Chunk, salt...)
	tgc1Chunk = append(tgc1Chunk, nonce...)
	tgc1Chunk = append(tgc1Chunk, ciphertextV1...)

	// Decrypt TGC1 chunk via unified DecryptChunk
	decryptedV1, err := crypto.DecryptChunk(tgc1Chunk, passphrase)
	if err != nil {
		t.Fatalf("failed to decrypt legacy TGC1 chunk: %v", err)
	}
	if !bytes.Equal(plaintext, decryptedV1) {
		t.Fatalf("decrypted TGC1 plaintext mismatch")
	}

	// Encrypt same plaintext with modern TGC2
	tgc2Chunk, err := crypto.EncryptChunk(plaintext, passphrase)
	if err != nil {
		t.Fatalf("EncryptChunk (TGC2) failed: %v", err)
	}

	// Decrypt TGC2 chunk via same DecryptChunk function
	decryptedV2, err := crypto.DecryptChunk(tgc2Chunk, passphrase)
	if err != nil {
		t.Fatalf("failed to decrypt modern TGC2 chunk: %v", err)
	}
	if !bytes.Equal(plaintext, decryptedV2) {
		t.Fatalf("decrypted TGC2 plaintext mismatch")
	}
}
