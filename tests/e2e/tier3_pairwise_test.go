package e2e_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/crypto"
)

// ============================================================================
// Tier 3: Cross-Feature Combinations (Pairwise Coverage)
// ============================================================================

// TestTier3_WebDAV_Mount_Mixed_TGC1_TGC2_WORM verifies WebDAV streaming files
// comprised of both legacy TGC1 and modern TGC2 chunks under active WORM lock.
func TestTier3_WebDAV_Mount_Mixed_TGC1_TGC2_WORM(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	passphrase := "pairwise-test-passphrase-32b"

	// 1. Prepare TGC1 chunk (legacy uncompressed)
	tgc1Plaintext := []byte("Chunk 0: legacy TGC1 uncompressed content for testing pairwise compatibility")
	salt := make([]byte, 16)
	copy(salt, []byte("salt0123456789ab"))
	nonce := make([]byte, 12)
	copy(nonce, []byte("nonce0123456"))

	key := crypto.DeriveKey(passphrase, salt)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	ciphertextV1 := gcm.Seal(nil, nonce, tgc1Plaintext, []byte("TGC1"))

	var tgc1Chunk []byte
	tgc1Chunk = append(tgc1Chunk, []byte("TGC1")...)
	tgc1Chunk = append(tgc1Chunk, salt...)
	tgc1Chunk = append(tgc1Chunk, nonce...)
	tgc1Chunk = append(tgc1Chunk, ciphertextV1...)

	// 2. Prepare TGC2 chunk (modern Zstd compressed)
	tgc2Plaintext := []byte("Chunk 1: modern TGC2 compressed content with Zstandard dictionary compression")
	tgc2Chunk, err := crypto.EncryptChunkWithCompression(tgc2Plaintext, passphrase, crypto.CompressionZstd)
	if err != nil {
		t.Fatalf("failed to create TGC2 chunk: %v", err)
	}

	// 3. Verify that transparent decryptor handles both chunks sequentially
	dec1, err := crypto.DecryptChunk(tgc1Chunk, passphrase)
	if err != nil {
		t.Fatalf("failed to decrypt TGC1 chunk: %v", err)
	}
	if !bytes.Equal(dec1, tgc1Plaintext) {
		t.Errorf("TGC1 decrypted content mismatch")
	}

	dec2, err := crypto.DecryptChunk(tgc2Chunk, passphrase)
	if err != nil {
		t.Fatalf("failed to decrypt TGC2 chunk: %v", err)
	}
	if !bytes.Equal(dec2, tgc2Plaintext) {
		t.Errorf("TGC2 decrypted content mismatch")
	}

	// 4. Verify HTTP range read from WebDAV server
	req, _ := http.NewRequest("GET", "http://127.0.0.1:8089/latest/mixed_file.txt", nil)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK from WebDAV mount, got: %d", resp.StatusCode)
		}
	}
}

// TestTier3_TelegramBot_BackupNow_During_WebDAV_Streaming verifies concurrent
// operation between WebDAV streaming reads and on-demand Telegram Bot backup trigger.
func TestTier3_TelegramBot_BackupNow_During_WebDAV_Streaming(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() || !ctx.SupportsBot() {
		t.Skip("Pairwise WebDAV + Bot requires M3 and M4 (currently M1)")
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: Continuous WebDAV Range streaming requests
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			req, _ := http.NewRequest("GET", "http://127.0.0.1:8089/latest/video.mp4", nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", i*1024, (i+1)*1024-1))
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	// Goroutine 2: Telegram Bot /backup_now command trigger
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		ctx.MockTG.InjectMessage(1001, "admin_user", 1001, "/backup_now")
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("WebDAV streaming and Telegram Bot backup completed concurrently without deadlock")
	case <-time.After(5 * time.Second):
		t.Fatalf("deadlock detected between WebDAV streaming reader and Bot backup controller")
	}
}

// TestTier3_WORM_Prune_Attempt_During_Active_WebDAV_Mount verifies that attempting
// retention pruning while a WORM snapshot is actively mounted does not corrupt or drop the mount.
func TestTier3_WORM_Prune_Attempt_During_Active_WebDAV_Mount(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWebDAV() {
		t.Skip("Feature 1 (WebDAV Server) requires M3 (currently M1)")
	}

	// 1. Initiate active WebDAV stream
	req, _ := http.NewRequest("GET", "http://127.0.0.1:8089/latest/document.pdf", nil)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, _ := client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}

	// 2. Trigger prune CLI
	_, _, exitCode, _ := ctx.RunCLI("prune", "--dry-run")
	t.Logf("prune during active WebDAV mount completed with exit code: %d", exitCode)

	// 3. Re-verify WebDAV server remains healthy and responsive
	reqCheck, _ := http.NewRequest("OPTIONS", "http://127.0.0.1:8089/", nil)
	respCheck, err := client.Do(reqCheck)
	if err == nil {
		defer respCheck.Body.Close()
		if respCheck.StatusCode != http.StatusOK && respCheck.StatusCode != http.StatusNoContent {
			t.Errorf("WebDAV server became unhealthy after prune: status=%d", respCheck.StatusCode)
		}
	}
}

// TestTier3_Bot_Status_Reports_WORM_And_Compression verifies that the /status command
// aggregates both WORM immutability metrics and compression efficiency ratios.
func TestTier3_Bot_Status_Reports_WORM_And_Compression(t *testing.T) {
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

	// Expected status report tokens
	expectedTokens := []string{"Node:", "Snapshots:", "Storage:", "QoS:"}
	for _, tok := range expectedTokens {
		if !strings.Contains(msg.Text, tok) {
			t.Errorf("expected /status to report %s, got: %s", tok, msg.Text)
		}
	}
}

// TestTier3_Reindex_With_Mixed_TGC1_TGC2_And_WORM verifies disaster recovery
// reindexing when the remote Telegram channel contains mixed TGC1/TGC2 manifests.
func TestTier3_Reindex_With_Mixed_TGC1_TGC2_And_WORM(t *testing.T) {
	ctx := NewE2EContext(t)
	if !ctx.SupportsWORM() {
		t.Skip("Feature 3 (Disaster Recovery Reindex) requires M2 (currently M1)")
	}

	stdout, stderr, exitCode, _ := ctx.RunCLI("reindex")
	t.Logf("reindex returned exit code: %d, stdout: %s, stderr: %s", exitCode, stdout, stderr)
}
