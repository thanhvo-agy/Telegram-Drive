package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"testing"
	"time"
)

// helper to produce legacy TGC1 encrypted chunk for backwards compatibility testing
func encryptLegacyTGC1(plaintext []byte, passphrase string) ([]byte, error) {
	salt := make([]byte, SaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}

	key := DeriveKey(passphrase, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, MagicHeader)

	out := make([]byte, 0, len(MagicHeader)+SaltSize+NonceSize+len(ciphertext))
	out = append(out, MagicHeader...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return out, nil
}

func TestEncryptDecryptTGC2Basic(t *testing.T) {
	passphrase := "rayma-super-secret-passphrase-123456"
	plaintext := []byte("Hello, this is a secret backup chunk payload representing a critical file.")

	encrypted, err := EncryptChunk(plaintext, passphrase)
	if err != nil {
		t.Fatalf("EncryptChunk failed: %v", err)
	}

	// Verify magic is TGC2
	if !bytes.Equal(encrypted[:4], MagicHeaderV2) {
		t.Fatalf("Expected magic TGC2, got: %q", encrypted[:4])
	}

	if bytes.Equal(encrypted, plaintext) {
		t.Fatal("Encrypted data is identical to plaintext!")
	}

	decrypted, err := DecryptChunk(encrypted, passphrase)
	if err != nil {
		t.Fatalf("DecryptChunk failed: %v", err)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("Decrypted data mismatch. Got %s, want %s", string(decrypted), string(plaintext))
	}

	// Test incorrect password
	_, err = DecryptChunk(encrypted, "wrong-password")
	if err == nil {
		t.Fatal("Expected error with wrong password, but got nil")
	}

	// Test corrupted header magic
	corrupted := append([]byte(nil), encrypted...)
	corrupted[0] = 'X'
	_, err = DecryptChunk(corrupted, passphrase)
	if err == nil {
		t.Fatal("Expected error with corrupted header, but got nil")
	}
}

func TestCompressibleTextSizeReduction(t *testing.T) {
	passphrase := "compress-test-passphrase-789"

	// Create compressible text: repetitive log/JSON lines
	var buf bytes.Buffer
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&buf, `{"timestamp":"2026-11-10T10:00:%02dZ","level":"INFO","event":"chunk_upload","file_id":"file_%06d","bytes_sent":52428800,"status":"OK"}`+"\n", i%60, i)
	}
	plaintext := buf.Bytes()
	origSize := len(plaintext)

	encrypted, err := EncryptChunk(plaintext, passphrase)
	if err != nil {
		t.Fatalf("EncryptChunk failed: %v", err)
	}

	encSize := len(encrypted)

	// Verify container magic is TGC2
	if !bytes.Equal(encrypted[:4], MagicHeaderV2) {
		t.Fatalf("Expected magic TGC2, got: %q", encrypted[:4])
	}

	// Verify Flag is CompressionZstd (0x01)
	flag := CompressionType(encrypted[4])
	if flag != CompressionZstd {
		t.Fatalf("Expected CompressionZstd flag (0x01), got: 0x%02x", flag)
	}

	// Verify OrigSz in header matches origSize
	origSzHeader := binary.BigEndian.Uint32(encrypted[5:9])
	if origSzHeader != uint32(origSize) {
		t.Fatalf("Expected OrigSz %d, got %d", origSize, origSzHeader)
	}

	// Calculate savings percentage
	savings := float64(origSize-encSize) / float64(origSize) * 100.0
	t.Logf("Compressible text: original=%d bytes, encrypted=%d bytes, reduction=%.2f%%", origSize, encSize, savings)

	// Requirement: Verify >= 30-50% size reduction
	if savings < 30.0 {
		t.Fatalf("Expected >= 30%% size reduction, achieved only %.2f%%", savings)
	}

	// Decrypt and verify exact match
	decrypted, err := DecryptChunk(encrypted, passphrase)
	if err != nil {
		t.Fatalf("DecryptChunk failed: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatal("Decrypted content did not match original plaintext!")
	}
}

func TestIncompressibleDataBypass(t *testing.T) {
	passphrase := "incompressible-passphrase-456"

	// High entropy pseudo-random bytes simulate pre-compressed media (JPEG, PNG, MP4, ZIP)
	rawMedia := make([]byte, 128*1024) // 128 KB
	if _, err := io.ReadFull(rand.Reader, rawMedia); err != nil {
		t.Fatalf("Failed to generate random bytes: %v", err)
	}

	encrypted, err := EncryptChunk(rawMedia, passphrase)
	if err != nil {
		t.Fatalf("EncryptChunk failed: %v", err)
	}

	// Verify container magic is TGC2
	if !bytes.Equal(encrypted[:4], MagicHeaderV2) {
		t.Fatalf("Expected magic TGC2, got: %q", encrypted[:4])
	}

	// Incompressible bypass: Flag MUST be CompressionNone (0x00)
	flag := CompressionType(encrypted[4])
	if flag != CompressionNone {
		t.Fatalf("Expected incompressible bypass flag 0x00 (CompressionNone), got: 0x%02x", flag)
	}

	// Container size must have 0% data inflation:
	// HeaderV2Size (9) + SaltSize (16) + NonceSize (12) + len(rawMedia) + GCMTagSize (16)
	expectedTotalSize := HeaderV2Size + SaltSize + NonceSize + len(rawMedia) + GCMTagSize
	if len(encrypted) != expectedTotalSize {
		t.Fatalf("Encrypted size has inflation! Got %d bytes, expected exact %d bytes", len(encrypted), expectedTotalSize)
	}

	// Decrypt and verify exact match
	decrypted, err := DecryptChunk(encrypted, passphrase)
	if err != nil {
		t.Fatalf("DecryptChunk failed: %v", err)
	}
	if !bytes.Equal(decrypted, rawMedia) {
		t.Fatal("Decrypted media did not match original random bytes!")
	}
}

func TestTGC1BackwardsCompatibility(t *testing.T) {
	passphrase := "legacy-passphrase-2024"
	plaintext := []byte("Legacy TGC1 backup archive chunk payload for Time Machine snapshot #1042.")

	// Encrypt using legacy TGC1 format
	tgc1Data, err := encryptLegacyTGC1(plaintext, passphrase)
	if err != nil {
		t.Fatalf("encryptLegacyTGC1 failed: %v", err)
	}

	// Verify magic is TGC1
	if !bytes.Equal(tgc1Data[:4], MagicHeader) {
		t.Fatalf("Expected TGC1 magic, got: %q", tgc1Data[:4])
	}

	// DecryptChunk must transparently detect and decrypt TGC1
	decrypted, err := DecryptChunk(tgc1Data, passphrase)
	if err != nil {
		t.Fatalf("DecryptChunk failed on legacy TGC1 chunk: %v", err)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("Decrypted TGC1 mismatch. Got %s, want %s", string(decrypted), string(plaintext))
	}

	// Verify wrong password on TGC1 also fails cleanly
	_, err = DecryptChunk(tgc1Data, "incorrect-passphrase")
	if err == nil {
		t.Fatal("Expected error decrypting TGC1 with wrong password, got nil")
	}

	// Corrupted TGC1 magic
	corrupted := append([]byte(nil), tgc1Data...)
	corrupted[0] = 'Z'
	_, err = DecryptChunk(corrupted, passphrase)
	if err == nil {
		t.Fatal("Expected error on corrupted TGC1 magic, got nil")
	}
}

func TestTamperDetection(t *testing.T) {
	passphrase := "tamper-protection-passphrase"
	plaintext := []byte("Sensitive payload that must fail verification if any metadata is altered.")

	encrypted, err := EncryptChunk(plaintext, passphrase)
	if err != nil {
		t.Fatalf("EncryptChunk failed: %v", err)
	}

	// 1. Mutate Magic (data[0..3])
	{
		corrupted := append([]byte(nil), encrypted...)
		corrupted[0] = 'B'
		_, err := DecryptChunk(corrupted, passphrase)
		if err == nil {
			t.Fatal("Tampered Magic should have failed decryption, but succeeded")
		}
	}

	// 2. Mutate Magic from TGC2 to TGC1
	{
		corrupted := append([]byte(nil), encrypted...)
		copy(corrupted[:4], MagicHeader)
		_, err := DecryptChunk(corrupted, passphrase)
		if err == nil {
			t.Fatal("Altered magic TGC2->TGC1 should fail GCM authentication, but succeeded")
		}
	}

	// 3. Mutate Compression Flag (data[4] - part of AAD)
	{
		corrupted := append([]byte(nil), encrypted...)
		corrupted[4] ^= 0x01 // flip flag bit
		_, err := DecryptChunk(corrupted, passphrase)
		if err == nil {
			t.Fatal("Tampered Flag in AAD should have caused GCM auth failure, but succeeded")
		}
	}

	// 4. Mutate OrigSz (data[5..8] - part of AAD)
	{
		corrupted := append([]byte(nil), encrypted...)
		corrupted[5] ^= 0xFF
		_, err := DecryptChunk(corrupted, passphrase)
		if err == nil {
			t.Fatal("Tampered OrigSz in AAD should have caused GCM auth failure, but succeeded")
		}
	}

	// 5. Mutate Salt (data[9..24])
	{
		corrupted := append([]byte(nil), encrypted...)
		corrupted[15] ^= 0xAA
		_, err := DecryptChunk(corrupted, passphrase)
		if err == nil {
			t.Fatal("Tampered Salt should have caused key mismatch / GCM failure, but succeeded")
		}
	}

	// 6. Mutate Nonce (data[25..36])
	{
		corrupted := append([]byte(nil), encrypted...)
		corrupted[30] ^= 0x55
		_, err := DecryptChunk(corrupted, passphrase)
		if err == nil {
			t.Fatal("Tampered Nonce should have caused GCM auth failure, but succeeded")
		}
	}

	// 7. Mutate Ciphertext (data[37..len-16])
	{
		corrupted := append([]byte(nil), encrypted...)
		corrupted[38] ^= 0x01
		_, err := DecryptChunk(corrupted, passphrase)
		if err == nil {
			t.Fatal("Tampered Ciphertext should have caused GCM auth failure, but succeeded")
		}
	}

	// 8. Mutate GCM Auth Tag (data[len-16..])
	{
		corrupted := append([]byte(nil), encrypted...)
		corrupted[len(corrupted)-1] ^= 0x01
		_, err := DecryptChunk(corrupted, passphrase)
		if err == nil {
			t.Fatal("Tampered Auth Tag should have caused GCM auth failure, but succeeded")
		}
	}

	// 9. Truncate chunk below minimum size
	{
		truncated := encrypted[:MinContainerV2Size-1]
		_, err := DecryptChunk(truncated, passphrase)
		if err == nil {
			t.Fatal("Truncated chunk should have returned error, but succeeded")
		}
	}
}

func TestZeroByteChunk(t *testing.T) {
	passphrase := "zero-byte-passphrase"
	emptyPlaintext := []byte{}

	// Encrypt default (Zstd requested)
	encrypted, err := EncryptChunk(emptyPlaintext, passphrase)
	if err != nil {
		t.Fatalf("EncryptChunk on empty slice failed: %v", err)
	}

	if !bytes.Equal(encrypted[:4], MagicHeaderV2) {
		t.Fatalf("Expected TGC2 magic, got: %q", encrypted[:4])
	}

	// Flag should be CompressionNone (0x00) for 0 bytes
	flag := CompressionType(encrypted[4])
	if flag != CompressionNone {
		t.Fatalf("Expected CompressionNone (0x00) for empty chunk, got: 0x%02x", flag)
	}

	// OrigSz should be 0
	origSz := binary.BigEndian.Uint32(encrypted[5:9])
	if origSz != 0 {
		t.Fatalf("Expected OrigSz 0, got %d", origSz)
	}

	// Decrypt
	decrypted, err := DecryptChunk(encrypted, passphrase)
	if err != nil {
		t.Fatalf("DecryptChunk on zero-byte chunk failed: %v", err)
	}
	if len(decrypted) != 0 {
		t.Fatalf("Expected 0 bytes decrypted, got %d", len(decrypted))
	}

	// Test with CompressionNone explicitly
	encNone, err := EncryptChunkWithCompression(emptyPlaintext, passphrase, CompressionNone)
	if err != nil {
		t.Fatalf("EncryptChunkWithCompression None failed: %v", err)
	}
	decNone, err := DecryptChunk(encNone, passphrase)
	if err != nil {
		t.Fatalf("DecryptChunk failed on explicit None: %v", err)
	}
	if len(decNone) != 0 {
		t.Fatalf("Expected 0 bytes decrypted, got %d", len(decNone))
	}
}

func TestLargeChunk49MB(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping 49MB test in short mode")
	}

	passphrase := "teleclone-49mb-chunk-passphrase"
	chunkSize := 49 * 1024 * 1024 // 49 MB

	// Generate 49MB of realistic semi-compressible data (repeating 1KB pattern with variations)
	t.Log("Generating 49MB test chunk...")
	rawChunk := make([]byte, chunkSize)
	pattern := []byte("TELECLONE_PRO_V2_49MB_SNAPSHOT_CHUNK_PATTERN_DATA_STREAM_TEST_ABC_1234567890_\n")
	for i := 0; i < chunkSize; i += len(pattern) {
		n := copy(rawChunk[i:], pattern)
		if n < len(pattern) {
			break
		}
	}
	// Add some variations
	for i := 0; i < chunkSize; i += 4096 {
		binary.LittleEndian.PutUint32(rawChunk[i:i+4], uint32(i))
	}

	origHash := HashData(rawChunk)

	startEnc := time.Now()
	encrypted, err := EncryptChunk(rawChunk, passphrase)
	if err != nil {
		t.Fatalf("EncryptChunk 49MB failed: %v", err)
	}
	encDuration := time.Since(startEnc)

	savings := float64(len(rawChunk)-len(encrypted)) / float64(len(rawChunk)) * 100.0
	t.Logf("49MB Chunk Encrypt: original=%d, encrypted=%d (%.2f%% savings) in %v",
		len(rawChunk), len(encrypted), savings, encDuration)

	if savings < 30.0 {
		t.Fatalf("Expected >= 30%% savings on 49MB semi-compressible data, got %.2f%%", savings)
	}

	startDec := time.Now()
	decrypted, err := DecryptChunk(encrypted, passphrase)
	if err != nil {
		t.Fatalf("DecryptChunk 49MB failed: %v", err)
	}
	decDuration := time.Since(startDec)

	t.Logf("49MB Chunk Decrypt: %d bytes verified in %v", len(decrypted), decDuration)

	if len(decrypted) != chunkSize {
		t.Fatalf("Decrypted size mismatch: got %d, want %d", len(decrypted), chunkSize)
	}

	decHash := HashData(decrypted)
	if decHash != origHash {
		t.Fatalf("Hash mismatch after 49MB decryption! got %s, want %s", decHash, origHash)
	}
}

func TestZeroGoroutineLeak(t *testing.T) {
	passphrase := "goroutine-leak-check-passphrase"
	data := bytes.Repeat([]byte("Check for goroutine leaks in compression and decryption pipeline.\n"), 500)

	runtime.GC()
	initialGoroutines := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		enc, err := EncryptChunk(data, passphrase)
		if err != nil {
			t.Fatalf("EncryptChunk failed: %v", err)
		}
		dec, err := DecryptChunk(enc, passphrase)
		if err != nil {
			t.Fatalf("DecryptChunk failed: %v", err)
		}
		if !bytes.Equal(dec, data) {
			t.Fatal("Mismatch in leak test")
		}
	}

	runtime.GC()
	finalGoroutines := runtime.NumGoroutine()

	// Ensure no background goroutines leaked
	if finalGoroutines > initialGoroutines {
		t.Fatalf("Goroutine leak detected! Initial: %d, Final: %d", initialGoroutines, finalGoroutines)
	}
}

func TestDirectCompressDecompress(t *testing.T) {
	data := []byte("The quick brown fox jumps over the lazy dog. Repetition repetition repetition.")
	compressed, err := CompressData(data)
	if err != nil {
		t.Fatalf("CompressData failed: %v", err)
	}

	decompressed, err := DecompressData(compressed, uint32(len(data)))
	if err != nil {
		t.Fatalf("DecompressData failed: %v", err)
	}
	if !bytes.Equal(decompressed, data) {
		t.Fatalf("Decompressed data mismatch: got %q, want %q", decompressed, data)
	}

	// Decompress with wrong origSz
	_, err = DecompressData(compressed, uint32(len(data)+10))
	if err == nil {
		t.Fatal("Expected error for wrong origSz, got nil")
	}

	// Corrupted compressed data
	corrupted := append([]byte(nil), compressed...)
	corrupted[0] = 0xFF
	_, err = DecompressData(corrupted, uint32(len(data)))
	if err == nil {
		t.Fatal("Expected error for corrupted compressed data, got nil")
	}

	// Empty data
	emptyComp, err := CompressData(nil)
	if err != nil {
		t.Fatalf("CompressData(nil) failed: %v", err)
	}
	emptyDecomp, err := DecompressData(emptyComp, 0)
	if err != nil {
		t.Fatalf("DecompressData(empty, 0) failed: %v", err)
	}
	if len(emptyDecomp) != 0 {
		t.Fatalf("Expected 0 bytes, got %d", len(emptyDecomp))
	}
}

func BenchmarkEncryptDecrypt(b *testing.B) {
	passphrase := "benchmark-passphrase-12345"
	data := bytes.Repeat([]byte("Benchmark repetitive log stream data for high throughput testing.\n"), 1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc, err := EncryptChunk(data, passphrase)
		if err != nil {
			b.Fatal(err)
		}
		_, err = DecryptChunk(enc, passphrase)
		if err != nil {
			b.Fatal(err)
		}
	}
}
