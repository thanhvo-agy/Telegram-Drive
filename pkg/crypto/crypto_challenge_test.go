package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Challenge 1: Compression ratio on varied real-world data patterns
func TestEmpiricalCompressionRatios(t *testing.T) {
	passphrase := "m1-challenger-compression-suite"

	patterns := []struct {
		name              string
		generator         func() []byte
		expectedFlag      CompressionType
		minSavingsPercent float64
		maxInflationBytes int
	}{
		{
			name: "Highly compressible: Structured JSON logs (1MB)",
			generator: func() []byte {
				var buf bytes.Buffer
				for i := 0; i < 10000; i++ {
					fmt.Fprintf(&buf, `{"timestamp":"2026-11-10T10:00:%02dZ","service":"tgdrive-sync","level":"DEBUG","chunk_id":%d,"hash":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","status":"OK"}`+"\n", i%60, i)
				}
				return buf.Bytes()
			},
			expectedFlag:      CompressionZstd,
			minSavingsPercent: 50.0,
			maxInflationBytes: 0,
		},
		{
			name: "Highly compressible: Repeated text & source code (500KB)",
			generator: func() []byte {
				codeSample := []byte(`
package main

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type Worker struct {
	ID        int
	JobQueue  chan Job
	WaitGroup *sync.WaitGroup
}

func (w *Worker) Start() {
	go func() {
		defer w.WaitGroup.Done()
		for job := range w.JobQueue {
			fmt.Printf("Worker %d processing job %d\n", w.ID, job.ID)
			time.Sleep(10 * time.Millisecond)
		}
	}()
}
`)
				return bytes.Repeat(codeSample, 1500)
			},
			expectedFlag:      CompressionZstd,
			minSavingsPercent: 70.0,
			maxInflationBytes: 0,
		},
		{
			name: "Semi-compressible: Simulated ELF executable binary (2MB)",
			generator: func() []byte {
				buf := make([]byte, 2*1024*1024)
				for i := 0; i < len(buf); i++ {
					buf[i] = byte((i*37 + 13) ^ (i >> 3))
				}
				strTable := []byte("runtime.main\x00runtime.goexit\x00tgdrive.BackupEngine\x00crypto.AESGCMEncrypt\x00telegram.MultiBotClient\x00")
				for i := 0; i < len(buf)-len(strTable); i += 8192 {
					copy(buf[i:], strTable)
				}
				for i := 4096; i < len(buf); i += 65536 {
					for j := 0; j < 1024 && i+j < len(buf); j++ {
						buf[i+j] = 0
					}
				}
				return buf
			},
			expectedFlag:      CompressionZstd,
			minSavingsPercent: 20.0,
			maxInflationBytes: 0,
		},
		{
			name: "Semi-compressible: Mixed CSV tabular data (1MB)",
			generator: func() []byte {
				var buf bytes.Buffer
				buf.WriteString("id,first_name,last_name,email,ip_address,created_at,balance,is_active\n")
				for i := 0; i < 15000; i++ {
					fmt.Fprintf(&buf, "%d,John_%d,Doe_%d,user_%d@example-domain.com,192.168.1.%d,2026-11-10T10:15:30Z,%.2f,%t\n",
						i, i%500, i%500, i, i%255, float64(i)*1.23, i%2 == 0)
				}
				return buf.Bytes()
			},
			expectedFlag:      CompressionZstd,
			minSavingsPercent: 60.0,
			maxInflationBytes: 0,
		},
		{
			name: "Incompressible: High-entropy cryptographic random bytes (1MB)",
			generator: func() []byte {
				b := make([]byte, 1024*1024)
				if _, err := io.ReadFull(rand.Reader, b); err != nil {
					panic(err)
				}
				return b
			},
			expectedFlag:      CompressionNone, // Bypass MUST engage
			minSavingsPercent: -0.01,
			maxInflationBytes: 0, // 0% inflation beyond the 53B envelope
		},
		{
			name: "Incompressible: Small random block (64KB)",
			generator: func() []byte {
				b := make([]byte, 64*1024)
				if _, err := io.ReadFull(rand.Reader, b); err != nil {
					panic(err)
				}
				return b
			},
			expectedFlag:      CompressionNone,
			minSavingsPercent: -0.01,
			maxInflationBytes: 0,
		},
		{
			name: "Edge Case: Zero bytes empty slice",
			generator: func() []byte {
				return []byte{}
			},
			expectedFlag:      CompressionNone,
			minSavingsPercent: -0.01,
			maxInflationBytes: 0,
		},
		{
			name: "Edge Case: Single byte (1B)",
			generator: func() []byte {
				return []byte{0x42}
			},
			expectedFlag:      CompressionNone,
			minSavingsPercent: -0.01,
			maxInflationBytes: 0,
		},
		{
			name: "Edge Case: All zeros (512KB)",
			generator: func() []byte {
				return make([]byte, 512*1024)
			},
			expectedFlag:      CompressionZstd,
			minSavingsPercent: 99.0,
			maxInflationBytes: 0,
		},
	}

	for _, p := range patterns {
		t.Run(p.name, func(t *testing.T) {
			raw := p.generator()
			rawLen := len(raw)

			encrypted, err := EncryptChunk(raw, passphrase)
			if err != nil {
				t.Fatalf("EncryptChunk failed: %v", err)
			}

			if len(encrypted) < MinContainerV2Size {
				t.Fatalf("Encrypted size %d < MinContainerV2Size (%d)", len(encrypted), MinContainerV2Size)
			}
			if !bytes.Equal(encrypted[:4], MagicHeaderV2) {
				t.Fatalf("Container magic is not TGC2, got: %q", encrypted[:4])
			}

			flag := CompressionType(encrypted[4])
			if flag != p.expectedFlag {
				t.Fatalf("Expected flag 0x%02x, got 0x%02x", p.expectedFlag, flag)
			}

			origSz := binary.BigEndian.Uint32(encrypted[5:9])
			if origSz != uint32(rawLen) {
				t.Fatalf("Header OrigSz mismatch: got %d, want %d", origSz, rawLen)
			}

			// Check inflation for uncompressed / incompressible
			if flag == CompressionNone {
				expectedSize := HeaderV2Size + SaltSize + NonceSize + rawLen + GCMTagSize
				if len(encrypted) != expectedSize {
					t.Fatalf("Inflation detected! Container is %d bytes, expected exact %d bytes (raw %d + 53)",
						len(encrypted), expectedSize, rawLen)
				}
			}

			// Check compression savings
			if p.minSavingsPercent > 0 {
				savings := float64(rawLen-len(encrypted)) / float64(rawLen) * 100.0
				t.Logf("[%s] Raw: %d bytes -> Encrypted: %d bytes (%.2f%% savings)", p.name, rawLen, len(encrypted), savings)
				if savings < p.minSavingsPercent {
					t.Fatalf("Insufficient savings: got %.2f%%, expected >= %.2f%%", savings, p.minSavingsPercent)
				}
			} else {
				t.Logf("[%s] Raw: %d bytes -> Encrypted: %d bytes (Bypassed Zstd as expected)", p.name, rawLen, len(encrypted))
			}

			// Verify round-trip decryption
			decrypted, err := DecryptChunk(encrypted, passphrase)
			if err != nil {
				t.Fatalf("DecryptChunk failed: %v", err)
			}
			if !bytes.Equal(decrypted, raw) {
				t.Fatalf("Decrypted content does not match original! Lengths: got %d, want %d", len(decrypted), rawLen)
			}
		})
	}
}

// Challenge 2: Throughput performance on 49MB chunk sizes across all data profiles
func TestEmpiricalThroughput49MB(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping 49MB stress test in short mode")
	}

	passphrase := "teleclone-49mb-stress-passphrase-2026"
	chunkSize := 49 * 1024 * 1024 // 51,380,224 bytes

	testCases := []struct {
		name       string
		dataGen    func() []byte
		minEncMBps float64
		minDecMBps float64
	}{
		{
			name: "49MB Compressible Text & Log Pattern",
			dataGen: func() []byte {
				buf := make([]byte, chunkSize)
				pattern := []byte("2026-11-10T10:00:00Z [INFO] worker_pool_upload_event_chunk_seq_0000000001 status=SUCCESS bytes=51380224\n")
				for i := 0; i < chunkSize; i += len(pattern) {
					copy(buf[i:], pattern)
				}
				return buf
			},
			minEncMBps: 50.0,  // Target: >= 50 MB/s (under 1 second per 49MB chunk)
			minDecMBps: 100.0, // Target: >= 100 MB/s (under 500ms per 49MB chunk)
		},
		{
			name: "49MB Semi-Compressible Binary Data",
			dataGen: func() []byte {
				buf := make([]byte, chunkSize)
				for i := 0; i < chunkSize; i++ {
					buf[i] = byte((i * 17) ^ (i >> 5))
				}
				sig := []byte("TGC2_CHUNK_SIGNATURE_BINARY_STREAM_MARKER_BLOCK_DATA_49MB")
				for i := 0; i < chunkSize; i += 2048 {
					copy(buf[i:], sig)
				}
				return buf
			},
			minEncMBps: 50.0,
			minDecMBps: 100.0,
		},
		{
			name: "49MB Incompressible High-Entropy Noise (Media)",
			dataGen: func() []byte {
				buf := make([]byte, chunkSize)
				seed := uint64(0x123456789abcdef0)
				for i := 0; i < chunkSize; i += 8 {
					seed = seed*6364136223846793005 + 1442695040888963407
					binary.LittleEndian.PutUint64(buf[i:], seed)
				}
				return buf
			},
			minEncMBps: 50.0,
			minDecMBps: 100.0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.dataGen()
			rawLen := len(raw)
			rawMB := float64(rawLen) / (1024 * 1024)

			// Benchmark Encryption
			runtime.GC()
			var memBefore runtime.MemStats
			runtime.ReadMemStats(&memBefore)

			startEnc := time.Now()
			encrypted, err := EncryptChunk(raw, passphrase)
			encDur := time.Since(startEnc)
			if err != nil {
				t.Fatalf("Encryption failed: %v", err)
			}

			var memAfter runtime.MemStats
			runtime.ReadMemStats(&memAfter)

			encMBps := rawMB / encDur.Seconds()
			savings := float64(rawLen-len(encrypted)) / float64(rawLen) * 100.0
			t.Logf("[%s Encrypt] Size: %.2f MB -> %.2f MB (%.2f%% savings) in %v (%.2f MB/s)",
				tc.name, rawMB, float64(len(encrypted))/(1024*1024), savings, encDur, encMBps)

			if encMBps < tc.minEncMBps {
				t.Errorf("Encryption throughput %.2f MB/s below threshold %.2f MB/s", encMBps, tc.minEncMBps)
			}

			// Benchmark Decryption
			startDec := time.Now()
			decrypted, err := DecryptChunk(encrypted, passphrase)
			decDur := time.Since(startDec)
			if err != nil {
				t.Fatalf("Decryption failed: %v", err)
			}

			decMBps := rawMB / decDur.Seconds()
			t.Logf("[%s Decrypt] Size: %.2f MB verified in %v (%.2f MB/s)",
				tc.name, rawMB, decDur, decMBps)

			if decMBps < tc.minDecMBps {
				t.Errorf("Decryption throughput %.2f MB/s below threshold %.2f MB/s", decMBps, tc.minDecMBps)
			}

			if !bytes.Equal(decrypted, raw) {
				t.Fatalf("Decrypted 49MB chunk data does not match original!")
			}
		})
	}
}

// Challenge 3: Exhaustive Cryptographic Tamper Resistance (Bit-flips across all header offsets and tag)
func TestEmpiricalTamperResistanceAllOffsets(t *testing.T) {
	passphrase := "adversarial-crypto-challenge-key-2026"
	plaintext := []byte("CRITICAL_DATABASE_TRANSACTION_LOG_RECORD_OFFSET_AUTHENTICATION_INTEGRITY")

	encrypted, err := EncryptChunk(plaintext, passphrase)
	if err != nil {
		t.Fatalf("EncryptChunk failed: %v", err)
	}

	containerLen := len(encrypted)
	if containerLen < MinContainerV2Size {
		t.Fatalf("Container too short: %d", containerLen)
	}

	totalHeaderSaltNonceBytes := HeaderV2Size + SaltSize + NonceSize // 9 + 16 + 12 = 37 bytes
	t.Logf("Testing exhaustive single-bit flips across all %d header/salt/nonce bytes (296 bit-flips)...", totalHeaderSaltNonceBytes)

	tamperCount := 0
	passedTamperDetections := 0

	// 1. Exhaustive single-bit flips across every byte of Magic, Flag, OrigSz, Salt, Nonce
	for byteOffset := 0; byteOffset < totalHeaderSaltNonceBytes; byteOffset++ {
		for bit := 0; bit < 8; bit++ {
			tamperCount++
			corrupted := append([]byte(nil), encrypted...)
			corrupted[byteOffset] ^= (1 << bit)

			decrypted, err := DecryptChunk(corrupted, passphrase)
			if err == nil {
				t.Fatalf("CRITICAL SECURITY VULNERABILITY: Tamper at byte %d (bit %d) undetected! Decrypted=%q",
					byteOffset, bit, string(decrypted))
			}
			passedTamperDetections++
		}
	}

	t.Logf("Tested %d header/salt/nonce bit flips. 100%% rejected.", passedTamperDetections)

	// 2. Exhaustive single-bit flips across the entire 16-byte GCM Authentication Tag
	tagStartOffset := containerLen - GCMTagSize
	tagBitFlips := 0
	for byteOffset := tagStartOffset; byteOffset < containerLen; byteOffset++ {
		for bit := 0; bit < 8; bit++ {
			tagBitFlips++
			corrupted := append([]byte(nil), encrypted...)
			corrupted[byteOffset] ^= (1 << bit)

			_, err := DecryptChunk(corrupted, passphrase)
			if err == nil {
				t.Fatalf("CRITICAL SECURITY VULNERABILITY: Tamper at GCM Tag byte %d (bit %d) undetected!",
					byteOffset, bit)
			}
		}
	}
	t.Logf("Tested %d GCM Tag bit flips. 100%% rejected.", tagBitFlips)

	// 3. Bit flips across ciphertext body (first, middle, last bytes)
	ciphertextStart := totalHeaderSaltNonceBytes
	ciphertextEnd := tagStartOffset
	testCipherOffsets := []int{
		ciphertextStart,
		ciphertextStart + (ciphertextEnd-ciphertextStart)/2,
		ciphertextEnd - 1,
	}

	for _, byteOffset := range testCipherOffsets {
		for bit := 0; bit < 8; bit++ {
			corrupted := append([]byte(nil), encrypted...)
			corrupted[byteOffset] ^= (1 << bit)

			_, err := DecryptChunk(corrupted, passphrase)
			if err == nil {
				t.Fatalf("CRITICAL SECURITY VULNERABILITY: Tamper in Ciphertext byte %d undetected!", byteOffset)
			}
		}
	}
	t.Logf("Ciphertext body tamper tests passed.")

	// 4. Malicious Container Exploit Scenarios
	t.Run("Exploit: Huge OrigSz (4GB buffer overflow / OOM attack)", func(t *testing.T) {
		corrupted := append([]byte(nil), encrypted...)
		binary.BigEndian.PutUint32(corrupted[5:9], 0xFFFFFFFF)

		// Must fail authentication at GCM.Open BEFORE attempting DecompressData allocation
		_, err := DecryptChunk(corrupted, passphrase)
		if err == nil {
			t.Fatal("Huge OrigSz should have failed decryption, but succeeded!")
		}
	})

	t.Run("Exploit: Zero OrigSz with non-empty payload", func(t *testing.T) {
		corrupted := append([]byte(nil), encrypted...)
		binary.BigEndian.PutUint32(corrupted[5:9], 0)

		_, err := DecryptChunk(corrupted, passphrase)
		if err == nil {
			t.Fatal("Zero OrigSz should have failed decryption, but succeeded!")
		}
	})

	t.Run("Exploit: Truncated containers at every byte from 0 to 52", func(t *testing.T) {
		for length := 0; length < MinContainerV2Size; length++ {
			truncated := encrypted[:length]
			_, err := DecryptChunk(truncated, passphrase)
			if err == nil {
				t.Fatalf("Truncated container of length %d succeeded unexpectedly!", length)
			}
		}
	})

	t.Run("Exploit: Appended trailer garbage", func(t *testing.T) {
		corrupted := append([]byte(nil), encrypted...)
		corrupted = append(corrupted, []byte("MALICIOUS_APPENDED_TRAILING_DATA")...)

		_, err := DecryptChunk(corrupted, passphrase)
		if err == nil {
			t.Fatal("Container with appended trailing bytes should have failed decryption, but succeeded!")
		}
	})

	t.Run("Exploit: Header swapping between different chunks", func(t *testing.T) {
		chunk1, _ := EncryptChunk([]byte("Payload Number One"), passphrase)
		chunk2, _ := EncryptChunk([]byte("Payload Number Two With Longer Length"), passphrase)

		spliced := append([]byte(nil), chunk1[:HeaderV2Size]...)
		spliced = append(spliced, chunk2[HeaderV2Size:]...)

		_, err := DecryptChunk(spliced, passphrase)
		if err == nil {
			t.Fatal("Spliced container should fail AAD verification, but succeeded!")
		}
	})
}

// Challenge 4: Concurrency and Race Safety under Multi-Worker Pool
func TestEmpiricalConcurrentPoolSafety(t *testing.T) {
	passphrase := "concurrent-test-key-12345"
	numWorkers := 8
	iterationsPerWorker := 15

	var wg sync.WaitGroup
	errCh := make(chan error, numWorkers*iterationsPerWorker)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte(fmt.Sprintf("Worker payload %d - data line to compress and encrypt.\n", workerID)), 200)

			for i := 0; i < iterationsPerWorker; i++ {
				enc, err := EncryptChunk(payload, passphrase)
				if err != nil {
					errCh <- fmt.Errorf("worker %d encrypt iter %d: %w", workerID, i, err)
					return
				}

				dec, err := DecryptChunk(enc, passphrase)
				if err != nil {
					errCh <- fmt.Errorf("worker %d decrypt iter %d: %w", workerID, i, err)
					return
				}

				if !bytes.Equal(dec, payload) {
					errCh <- fmt.Errorf("worker %d payload mismatch at iter %d", workerID, i)
					return
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}
}
