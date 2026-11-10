package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"time"
)

// helper to encrypt custom payload with valid AES-GCM tag and specified AAD
func sealWithValidGCM(payload []byte, passphrase string, compType CompressionType, origSz uint32) ([]byte, error) {
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

	aad := make([]byte, HeaderV2Size)
	copy(aad[0:4], MagicHeaderV2)
	aad[4] = byte(compType)
	binary.BigEndian.PutUint32(aad[5:9], origSz)

	ciphertext := gcm.Seal(nil, nonce, payload, aad)

	out := make([]byte, 0, HeaderV2Size+SaltSize+NonceSize+len(ciphertext))
	out = append(out, aad...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return out, nil
}

// TestChallengerConcurrentStress stress-tests concurrent chunk encryption and decryption
// across multiple parallel goroutines with diverse payload types.
func TestChallengerConcurrentStress(t *testing.T) {
	concurrency := 50
	iterations := 20
	passphrase := "concurrent-stress-passphrase-2026"

	var wg sync.WaitGroup
	errCh := make(chan error, concurrency*iterations)

	for workerID := 0; workerID < concurrency; workerID++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()

			for iter := 0; iter < iterations; iter++ {
				var data []byte
				switch (wid + iter) % 4 {
				case 0:
					// Compressible repetitive data
					data = bytes.Repeat([]byte(fmt.Sprintf("worker-%d-iter-%d-payload-data-line\n", wid, iter)), 200)
				case 1:
					// Incompressible random data
					data = make([]byte, 8192)
					if _, err := io.ReadFull(rand.Reader, data); err != nil {
						errCh <- fmt.Errorf("worker %d: random gen failed: %w", wid, err)
						return
					}
				case 2:
					// Zero-byte data
					data = []byte{}
				case 3:
					// Mixed structured data (JSON-like)
					data = []byte(fmt.Sprintf(`{"worker":%d,"iteration":%d,"timestamp":"%s","data":"%s"}`,
						wid, iter, time.Now().UTC().Format(time.RFC3339Nano), bytes.Repeat([]byte("X"), 512)))
				}

				// Encrypt
				enc, err := EncryptChunk(data, passphrase)
				if err != nil {
					errCh <- fmt.Errorf("worker %d iter %d: EncryptChunk failed: %w", wid, iter, err)
					return
				}

				// Decrypt
				dec, err := DecryptChunk(enc, passphrase)
				if err != nil {
					errCh <- fmt.Errorf("worker %d iter %d: DecryptChunk failed: %w", wid, iter, err)
					return
				}

				// Validate exact match
				if !bytes.Equal(dec, data) {
					errCh <- fmt.Errorf("worker %d iter %d: payload mismatch! len(dec)=%d len(orig)=%d",
						wid, iter, len(dec), len(data))
					return
				}
			}
		}(workerID)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		t.Fatalf("Concurrent stress test encountered %d errors! First error: %v", len(errs), errs[0])
	}
}

// TestChallengerGoroutineLeak confirms zero lingering background goroutines
// after high-concurrency burst operations.
func TestChallengerGoroutineLeak(t *testing.T) {
	passphrase := "leak-test-passphrase-999"

	// Settle runtime
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	initialGoroutines := runtime.NumGoroutine()

	concurrency := 40
	opsPerWorker := 25
	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerWorker; j++ {
				payload := bytes.Repeat([]byte(fmt.Sprintf("leak-payload-%d-%d", id, j)), 100)
				enc, err := EncryptChunk(payload, passphrase)
				if err != nil {
					t.Errorf("EncryptChunk failed: %v", err)
					return
				}
				dec, err := DecryptChunk(enc, passphrase)
				if err != nil {
					t.Errorf("DecryptChunk failed: %v", err)
					return
				}
				if !bytes.Equal(dec, payload) {
					t.Errorf("Data mismatch")
					return
				}
			}
		}(i)
	}

	wg.Wait()

	// Settle and collect
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	finalGoroutines := runtime.NumGoroutine()
	if finalGoroutines > initialGoroutines {
		buf := make([]byte, 1024*64)
		n := runtime.Stack(buf, true)
		t.Fatalf("Goroutine leak detected: initial=%d, final=%d (+%d leaked goroutines)\nStack trace:\n%s",
			initialGoroutines, finalGoroutines, finalGoroutines-initialGoroutines, string(buf[:n]))
	}
}

// TestChallengerMemoryLeakAndBaseline tests that memory footprint returns to baseline
// and does not grow monotonically under continuous encrypt/decrypt cycles.
func TestChallengerMemoryLeakAndBaseline(t *testing.T) {
	passphrase := "memory-baseline-passphrase"
	data := bytes.Repeat([]byte("TeleClone Pro Memory Baseline Empirical Stress Test Line Content\n"), 1000)

	// Warm up pools
	for i := 0; i < 10; i++ {
		enc, _ := EncryptChunk(data, passphrase)
		_, _ = DecryptChunk(enc, passphrase)
	}

	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(50 * time.Millisecond)

	var mBefore runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	// Perform 100 iterations of encryption and decryption
	for i := 0; i < 100; i++ {
		enc, err := EncryptChunk(data, passphrase)
		if err != nil {
			t.Fatalf("iteration %d: EncryptChunk failed: %v", i, err)
		}
		dec, err := DecryptChunk(enc, passphrase)
		if err != nil {
			t.Fatalf("iteration %d: DecryptChunk failed: %v", i, err)
		}
		if len(dec) != len(data) {
			t.Fatalf("iteration %d: size mismatch", i)
		}
	}

	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(50 * time.Millisecond)

	var mAfter runtime.MemStats
	runtime.ReadMemStats(&mAfter)

	t.Logf("Memory stats: Before HeapAlloc=%d KB, After HeapAlloc=%d KB, Diff=%d KB",
		mBefore.HeapAlloc/1024, mAfter.HeapAlloc/1024, int64(mAfter.HeapAlloc-mBefore.HeapAlloc)/1024)

	// Check that residual heap growth is negligible (< 2 MB for 100 ops of 65KB chunks)
	heapGrowth := int64(mAfter.HeapAlloc) - int64(mBefore.HeapAlloc)
	if heapGrowth > 2*1024*1024 {
		t.Fatalf("Uncontrolled memory growth detected: %d bytes growth after GC", heapGrowth)
	}
}

// TestChallengerCorruptedCompressedWithValidTag tests edge cases where
// AES-GCM authentication succeeds, but decrypted data is corrupted zstd stream.
func TestChallengerCorruptedCompressedWithValidTag(t *testing.T) {
	passphrase := "valid-tag-corrupt-payload-passphrase"

	scenarios := []struct {
		name         string
		payload      []byte
		origSz       uint32
		expectedErr  error
	}{
		{
			name:        "Completely random bytes as zstd stream",
			payload:     []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
			origSz:      100,
			expectedErr: ErrDecompressFailed,
		},
		{
			name:        "Truncated zstd magic header (only 2 bytes)",
			payload:     []byte{0x28, 0xB5},
			origSz:      50,
			expectedErr: ErrDecompressFailed,
		},
		{
			name:        "Valid zstd magic header but corrupted frame descriptor",
			payload:     []byte{0x28, 0xB5, 0x2F, 0xFD, 0xFF, 0xFF, 0xFF},
			origSz:      200,
			expectedErr: ErrDecompressFailed,
		},
		{
			name:        "Empty payload with non-zero origSz",
			payload:     []byte{},
			origSz:      100,
			expectedErr: ErrDecompressFailed,
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			// Seal with valid GCM tag, but corrupted zstd payload
			chunk, err := sealWithValidGCM(sc.payload, passphrase, CompressionZstd, sc.origSz)
			if err != nil {
				t.Fatalf("sealWithValidGCM failed: %v", err)
			}

			// DecryptChunk must reach DecompressData and return ErrDecompressFailed
			_, err = DecryptChunk(chunk, passphrase)
			if err == nil {
				t.Fatal("Expected error on corrupted zstd payload with valid GCM tag, but got nil")
			}

			if !errors.Is(err, sc.expectedErr) {
				t.Fatalf("Expected error wrapping %v, got %v", sc.expectedErr, err)
			}
		})
	}
}

// TestChallengerMismatchedOrigSzWithValidTag tests what happens when
// valid zstd data is decrypted, but origSz in AAD does not match the actual uncompressed size.
func TestChallengerMismatchedOrigSzWithValidTag(t *testing.T) {
	passphrase := "mismatched-origsz-passphrase"
	raw := []byte("The actual plaintext message content that will be compressed.")

	compressed, err := CompressData(raw)
	if err != nil {
		t.Fatalf("CompressData failed: %v", err)
	}

	// Case 1: origSz smaller than actual uncompressed size
	t.Run("origSz smaller than real uncompressed size", func(t *testing.T) {
		wrongSz := uint32(len(raw) / 2)
		chunk, err := sealWithValidGCM(compressed, passphrase, CompressionZstd, wrongSz)
		if err != nil {
			t.Fatalf("seal failed: %v", err)
		}
		_, err = DecryptChunk(chunk, passphrase)
		if err == nil {
			t.Fatal("Expected error due to size mismatch, got nil")
		}
		if !errors.Is(err, ErrDecompressFailed) {
			t.Fatalf("Expected ErrDecompressFailed, got %v", err)
		}
	})

	// Case 2: origSz larger than actual uncompressed size
	t.Run("origSz larger than real uncompressed size", func(t *testing.T) {
		wrongSz := uint32(len(raw) * 2)
		chunk, err := sealWithValidGCM(compressed, passphrase, CompressionZstd, wrongSz)
		if err != nil {
			t.Fatalf("seal failed: %v", err)
		}
		_, err = DecryptChunk(chunk, passphrase)
		if err == nil {
			t.Fatal("Expected error due to size mismatch, got nil")
		}
		if !errors.Is(err, ErrDecompressFailed) {
			t.Fatalf("Expected ErrDecompressFailed, got %v", err)
		}
	})
}

// TestChallengerDecoderPoolPoisoning tests that if DecompressData fails on corrupted data,
// the decoder returned to decoderPool is NOT poisoned and can successfully decode valid data.
func TestChallengerDecoderPoolPoisoning(t *testing.T) {
	passphrase := "pool-poison-passphrase"
	validPlaintext := []byte("Valid plaintext data stream to verify decoder pool hygiene after failure.")

	validChunk, err := EncryptChunk(validPlaintext, passphrase)
	if err != nil {
		t.Fatalf("EncryptChunk failed: %v", err)
	}

	corruptPayload := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04}
	corruptChunk, err := sealWithValidGCM(corruptPayload, passphrase, CompressionZstd, 100)
	if err != nil {
		t.Fatalf("sealWithValidGCM failed: %v", err)
	}

	// Interleave corrupt and valid decryptions across 50 iterations
	for i := 0; i < 50; i++ {
		// Attempt decrypting corrupted chunk -> MUST fail
		_, err := DecryptChunk(corruptChunk, passphrase)
		if err == nil {
			t.Fatalf("iter %d: Expected error on corrupt chunk, got nil", i)
		}

		// Immediately decrypt valid chunk -> MUST succeed and match
		dec, err := DecryptChunk(validChunk, passphrase)
		if err != nil {
			t.Fatalf("iter %d: Decoder was poisoned by prior error! DecryptChunk failed: %v", i, err)
		}
		if !bytes.Equal(dec, validPlaintext) {
			t.Fatalf("iter %d: Decoder produced corrupted output on valid data! Got %q", i, dec)
		}
	}
}

// TestChallengerExtremeOrigSz tests how DecompressData behaves when origSz
// is extremely large (e.g. decompression bomb or malicious uint32).
func TestChallengerExtremeOrigSz(t *testing.T) {
	passphrase := "extreme-origsz-passphrase"

	// Create a valid small compressed payload
	smallData := []byte("Small string")
	compressed, err := CompressData(smallData)
	if err != nil {
		t.Fatalf("CompressData failed: %v", err)
	}

	t.Run("100MB origSz", func(t *testing.T) {
		origSz := uint32(100 * 1024 * 1024)
		chunk, err := sealWithValidGCM(compressed, passphrase, CompressionZstd, origSz)
		if err != nil {
			t.Fatalf("seal failed: %v", err)
		}

		_, err = DecryptChunk(chunk, passphrase)
		if err == nil {
			t.Fatal("Expected error for 100MB origSz with small compressed data, got nil")
		}
		if !errors.Is(err, ErrDecompressFailed) {
			t.Fatalf("Expected ErrDecompressFailed, got: %v", err)
		}
	})

	t.Run("MaxUint32 origSz", func(t *testing.T) {
		origSz := uint32(math.MaxUint32)
		chunk, err := sealWithValidGCM(compressed, passphrase, CompressionZstd, origSz)
		if err != nil {
			t.Fatalf("seal failed: %v", err)
		}

		// make([]byte, 0, math.MaxUint32) might allocate 4GB or fail. Let's see how it behaves safely.
		defer func() {
			if r := recover(); r != nil {
				t.Logf("Recovered from panic on MaxUint32 origSz: %v", r)
				// An unchecked panic on external data is a security finding!
			}
		}()

		_, err = DecryptChunk(chunk, passphrase)
		if err == nil {
			t.Fatal("Expected error for MaxUint32 origSz with small compressed data, got nil")
		}
	})
}
