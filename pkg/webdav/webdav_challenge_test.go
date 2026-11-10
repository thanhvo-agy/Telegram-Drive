package webdav

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/crypto"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/qos"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/snapshot"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/telegram"
)

// setupMockTelegramWithLatency creates a mock Telegram API server with configurable
// latency and request counting for rigorous concurrency and single-flight verification.
func setupMockTelegramWithLatency(t *testing.T, latency time.Duration, downloadCount *int64) (*httptest.Server, map[string][]byte, *sync.Mutex) {
	uploadedFiles := make(map[string][]byte)
	var mu sync.Mutex

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if latency > 0 {
			time.Sleep(latency)
		}

		mu.Lock()
		defer mu.Unlock()

		if strings.Contains(r.URL.Path, "getFile") {
			fileID := r.URL.Query().Get("file_id")
			data := uploadedFiles[fileID]
			resp := map[string]interface{}{
				"ok": true,
				"result": map[string]interface{}{
					"file_id":   fileID,
					"file_path": "documents/" + fileID,
					"file_size": len(data),
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if strings.HasPrefix(r.URL.Path, "/file/bot") {
			if downloadCount != nil {
				atomic.AddInt64(downloadCount, 1)
			}
			parts := strings.Split(r.URL.Path, "/")
			fileID := parts[len(parts)-1]
			if data, ok := uploadedFiles[fileID]; ok {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(data)
				return
			}
			http.NotFound(w, r)
			return
		}

		resp := map[string]interface{}{"ok": true, "result": true}
		_ = json.NewEncoder(w).Encode(resp)
	}))

	t.Setenv("TELEGRAM_API_URL", mockServer.URL)
	t.Setenv("HTTP_PROXY", mockServer.URL)
	t.Setenv("HTTPS_PROXY", mockServer.URL)

	return mockServer, uploadedFiles, &mu
}

// createEngineForTest initializes an engine and telegram client pointed at the mock server.
func createEngineForTest(t *testing.T, mockURL string, passphrase string) (*snapshot.Engine, *telegram.Client, string) {
	tempDir := t.TempDir()
	sourceDir := filepath.Join(tempDir, "source")
	stateDir := filepath.Join(tempDir, "snapshots")
	_ = os.MkdirAll(sourceDir, 0755)
	_ = os.MkdirAll(stateDir, 0755)

	cfg := &config.Config{
		BotTokens:     []string{"12345:mock_challenge_token"},
		ChannelID:     "-1001999888777",
		EncryptionKey: passphrase,
		SourcePath:    sourceDir,
		ChunkSize:     49 * 1024 * 1024,
		StateDir:      stateDir,
	}

	throttler, _ := qos.ParseBandwidthSchedule("00:00,100M")
	tgClient, err := telegram.NewClient(cfg.BotTokens, cfg.ChannelID, throttler)
	if err != nil {
		t.Fatalf("failed to create telegram client: %v", err)
	}

	engine, err := snapshot.NewEngine(cfg, tgClient)
	if err != nil {
		t.Fatalf("failed to create snapshot engine: %v", err)
	}

	return engine, tgClient, stateDir
}

// =========================================================================
// TEST 1: Seeking Torture - Random Seeks, Boundary Conditions & Contract
// =========================================================================

func TestChallenge_SeekingTorture_RapidRandomSeeks(t *testing.T) {
	passphrase := "webdav-challenge-passphrase-32b"
	const chunkCount = 5
	const chunkSize = 50000 // 50 KB each -> 250 KB total
	const totalSize = chunkCount * chunkSize

	// Generate deterministic ground truth data
	allData := make([]byte, totalSize)
	for i := 0; i < totalSize; i++ {
		allData[i] = byte((i*31 + 17) % 251)
	}

	cache := NewChunkCache(10, 60*time.Second)
	defer cache.Close()

	var chunks []snapshot.ChunkRef
	for i := 0; i < chunkCount; i++ {
		chunkStart := i * chunkSize
		chunkEnd := chunkStart + chunkSize
		chunkPlain := allData[chunkStart:chunkEnd]
		fileID := fmt.Sprintf("seek_chunk_%d", i)

		cache.Put(fileID, chunkPlain)

		chunks = append(chunks, snapshot.ChunkRef{
			Index:          i,
			Offset:         int64(chunkStart),
			Size:           int64(chunkSize),
			EncryptedSize:  int64(chunkSize + 53),
			Hash:           crypto.HashData(chunkPlain),
			TelegramFileID: fileID,
		})
	}

	fe := snapshot.FileEntry{
		Path:     "torture.bin",
		Size:     int64(totalSize),
		ModTime:  time.Now().Unix(),
		FileHash: crypto.HashData(allData),
		Chunks:   chunks,
	}

	f := newWebdavRegularFile(context.Background(), "torture.bin", fe, nil, cache, passphrase)
	defer f.Close()

	// 1. Initial State: cursor must be at 0
	if f.offset != 0 {
		t.Fatalf("initial offset expected 0, got: %d", f.offset)
	}

	// 2. Perform 1,000 randomized seek & read operations
	curPos := int64(0)
	for iter := 0; iter < 1000; iter++ {
		whenceChoice, _ := rand.Int(rand.Reader, big.NewInt(3))
		whence := int(whenceChoice.Int64()) // 0: SeekStart, 1: SeekCurrent, 2: SeekEnd

		var targetOffset int64
		switch whence {
		case io.SeekStart:
			n, _ := rand.Int(rand.Reader, big.NewInt(totalSize+10000))
			targetOffset = n.Int64()
		case io.SeekCurrent:
			n, _ := rand.Int(rand.Reader, big.NewInt(60000))
			// delta between -30,000 and +30,000
			delta := n.Int64() - 30000
			targetOffset = delta
		case io.SeekEnd:
			n, _ := rand.Int(rand.Reader, big.NewInt(60000))
			// delta between -60,000 and +10,000
			delta := n.Int64() - 50000
			targetOffset = delta
		}

		newPos, err := f.Seek(targetOffset, whence)

		// Calculate expected position
		var expectedPos int64
		switch whence {
		case io.SeekStart:
			expectedPos = targetOffset
		case io.SeekCurrent:
			expectedPos = curPos + targetOffset
		case io.SeekEnd:
			expectedPos = int64(totalSize) + targetOffset
		}

		if expectedPos < 0 {
			// Negative seek must return an error and NOT modify cursor
			if err == nil {
				t.Fatalf("iter %d: expected error for negative seek (%d, whence %d), got nil", iter, targetOffset, whence)
			}
			if f.offset != curPos {
				t.Fatalf("iter %d: negative seek modified offset from %d to %d", iter, curPos, f.offset)
			}
			continue
		}

		if err != nil {
			t.Fatalf("iter %d: unexpected seek error: %v", iter, err)
		}
		if newPos != expectedPos {
			t.Fatalf("iter %d: seek returned %d, expected %d", iter, newPos, expectedPos)
		}
		curPos = expectedPos

		// Read random buffer size between 1 and 8192 bytes
		readSzBig, _ := rand.Int(rand.Reader, big.NewInt(8192))
		readSz := int(readSzBig.Int64()) + 1
		buf := make([]byte, readSz)

		n, readErr := f.Read(buf)
		if curPos >= int64(totalSize) {
			// Cursor is at or past EOF: must return 0, io.EOF
			if readErr != io.EOF || n != 0 {
				t.Fatalf("iter %d: expected 0, io.EOF past EOF (pos %d); got n=%d, err=%v", iter, curPos, n, readErr)
			}
			continue
		}

		// Cursor was within valid file: must read min(readSz, totalSize - curPos) bytes
		expectedRead := int64(readSz)
		if expectedRead > int64(totalSize)-curPos {
			expectedRead = int64(totalSize) - curPos
		}

		if int64(n) != expectedRead {
			t.Fatalf("iter %d: pos %d, expected to read %d bytes, got %d (err: %v)", iter, curPos, expectedRead, n, readErr)
		}

		expectedSlice := allData[curPos : curPos+int64(n)]
		if !bytes.Equal(buf[:n], expectedSlice) {
			t.Fatalf("iter %d: data mismatch at pos %d, read %d bytes", iter, curPos, n)
		}

		curPos += int64(n)
	}

	// 3. Torture: Seek past EOF, verify EOF, seek back to byte 0, verify clean read
	_, err := f.Seek(int64(totalSize)+50000, io.SeekStart)
	if err != nil {
		t.Fatalf("seek past EOF failed: %v", err)
	}
	eofBuf := make([]byte, 100)
	n, err := f.Read(eofBuf)
	if err != io.EOF || n != 0 {
		t.Fatalf("read past EOF expected 0, io.EOF; got %d, %v", n, err)
	}

	// Rewind to 0
	pos, err := f.Seek(0, io.SeekStart)
	if err != nil || pos != 0 {
		t.Fatalf("rewind to 0 failed: %v", err)
	}
	firstBytes := make([]byte, 10)
	n, err = f.Read(firstBytes)
	if err != nil || n != 10 || !bytes.Equal(firstBytes, allData[:10]) {
		t.Fatalf("read after rewind failed: %v", err)
	}
}

// =========================================================================
// TEST 2: Cross-Chunk Reads - Spanning Multiple Chunk Boundaries
// =========================================================================

func TestChallenge_CrossChunkReads_MultiBoundarySpanning(t *testing.T) {
	passphrase := "webdav-challenge-passphrase-32b"

	// 4 chunks with different sizes: 40KB, 50KB, 60KB, 30KB = 180KB total
	chunkSizes := []int{40000, 50000, 60000, 30000}
	totalSize := 180000

	allData := make([]byte, totalSize)
	for i := 0; i < totalSize; i++ {
		allData[i] = byte((i * 47) % 256)
	}

	cache := NewChunkCache(10, 60*time.Second)
	defer cache.Close()

	var chunks []snapshot.ChunkRef
	curOffset := 0
	for i, sz := range chunkSizes {
		chunkData := allData[curOffset : curOffset+sz]
		fileID := fmt.Sprintf("multi_chunk_%d", i)
		cache.Put(fileID, chunkData)

		chunks = append(chunks, snapshot.ChunkRef{
			Index:          i,
			Offset:         int64(curOffset),
			Size:           int64(sz),
			EncryptedSize:  int64(sz + 53),
			Hash:           crypto.HashData(chunkData),
			TelegramFileID: fileID,
		})
		curOffset += sz
	}

	fe := snapshot.FileEntry{
		Path:     "multichunk.bin",
		Size:     int64(totalSize),
		ModTime:  time.Now().Unix(),
		FileHash: crypto.HashData(allData),
		Chunks:   chunks,
	}

	f := newWebdavRegularFile(context.Background(), "multichunk.bin", fe, nil, cache, passphrase)
	defer f.Close()

	// Scenario A: Read spanning exactly 2 chunks (Chunk 0 & Chunk 1)
	// Chunk 0 ends at 40,000. Start at 39,990 (10 bytes from chunk 0) and read 25 bytes (15 from chunk 1).
	_, _ = f.Seek(39990, io.SeekStart)
	buf2 := make([]byte, 25)
	n, err := f.Read(buf2)
	if err != nil || n != 25 {
		t.Fatalf("cross-2-chunks read failed: n=%d err=%v", n, err)
	}
	if !bytes.Equal(buf2, allData[39990:39990+25]) {
		t.Fatalf("cross-2-chunks data mismatch")
	}

	// Scenario B: Read spanning 3 CHUNKS in a single Read() call!
	// Chunk 0 ends at 40,000; Chunk 1 spans 40,000..90,000; Chunk 2 spans 90,000..150,000.
	// Start at 35,000 and read 65,000 bytes:
	// - 5,000 bytes from Chunk 0
	// - 50,000 bytes (ALL) from Chunk 1
	// - 10,000 bytes from Chunk 2
	_, _ = f.Seek(35000, io.SeekStart)
	buf3 := make([]byte, 65000)
	n, err = f.Read(buf3)
	if err != nil || n != 65000 {
		t.Fatalf("cross-3-chunks read failed: n=%d err=%v", n, err)
	}
	if !bytes.Equal(buf3, allData[35000:35000+65000]) {
		t.Fatalf("cross-3-chunks data mismatch")
	}

	// Scenario C: Read spanning ALL 4 CHUNKS in a single Read() call!
	// Start at offset 0 with a 200,000-byte buffer. Must read entire 180,000 bytes across all 4 chunks.
	_, _ = f.Seek(0, io.SeekStart)
	bufAll := make([]byte, 200000)
	n, err = f.Read(bufAll)
	if err != nil && err != io.EOF {
		t.Fatalf("cross-all-chunks read error: %v", err)
	}
	if n != totalSize {
		t.Fatalf("expected to read all %d bytes, got %d", totalSize, n)
	}
	if !bytes.Equal(bufAll[:n], allData) {
		t.Fatalf("cross-all-chunks content mismatch")
	}

	// Scenario D: Boundary edge read: 1 byte before chunk boundary, len 2
	// Chunk 1 ends at 90,000. Read at 89,999 with len 2.
	_, _ = f.Seek(89999, io.SeekStart)
	bufEdge := make([]byte, 2)
	n, err = f.Read(bufEdge)
	if err != nil || n != 2 {
		t.Fatalf("edge boundary read failed: n=%d err=%v", n, err)
	}
	if !bytes.Equal(bufEdge, allData[89999:90001]) {
		t.Fatalf("edge boundary content mismatch")
	}

	// Scenario E: Simulated 49MB chunk boundary mapping
	// Verify mathematical offset mapping across 49MB boundary (51,380,224 bytes)
	const boundary49MB = 49 * 1024 * 1024
	chunk49_0 := []byte("END_OF_CHUNK_0_BYTES_BEFORE_49MB")
	chunk49_1 := []byte("START_OF_CHUNK_1_BYTES_AFTER_49MB")

	cache49 := NewChunkCache(4, 60*time.Second)
	defer cache49.Close()
	cache49.Put("c49_0", chunk49_0)
	cache49.Put("c49_1", chunk49_1)

	fe49 := snapshot.FileEntry{
		Path: "huge_49mb.bin",
		Size: boundary49MB + int64(len(chunk49_1)),
		Chunks: []snapshot.ChunkRef{
			{Index: 0, Offset: boundary49MB - int64(len(chunk49_0)), Size: int64(len(chunk49_0)), TelegramFileID: "c49_0"},
			{Index: 1, Offset: boundary49MB, Size: int64(len(chunk49_1)), TelegramFileID: "c49_1"},
		},
	}

	f49 := newWebdavRegularFile(context.Background(), "huge_49mb.bin", fe49, nil, cache49, passphrase)
	defer f49.Close()

	// Seek to 5 bytes before 49MB boundary, read 10 bytes spanning across boundary
	_, _ = f49.Seek(boundary49MB-5, io.SeekStart)
	spanBuf := make([]byte, 10)
	n, err = f49.Read(spanBuf)
	if err != nil || n != 10 {
		t.Fatalf("49MB boundary cross read failed: n=%d, err=%v", n, err)
	}
	expectedSpan := append(chunk49_0[len(chunk49_0)-5:], chunk49_1[:5]...)
	if !bytes.Equal(spanBuf, expectedSpan) {
		t.Fatalf("49MB boundary content mismatch: got %q, expected %q", string(spanBuf), string(expectedSpan))
	}
}

// =========================================================================
// TEST 3: Mixed TGC1 and TGC2 Streaming Decryption Torture
// =========================================================================

func TestChallenge_Mixed_TGC1_TGC2_StreamingDecryption(t *testing.T) {
	passphrase := "webdav-challenge-passphrase-32b"
	const chunkCount = 6
	const chunkSize = 20000 // 20 KB per chunk -> 120 KB total
	const totalSize = chunkCount * chunkSize

	allData := make([]byte, totalSize)
	for i := 0; i < totalSize; i++ {
		allData[i] = byte((i*73 + 41) % 256)
	}

	var downloadCount int64
	mockTG, uploaded, mu := setupMockTelegramWithLatency(t, 0, &downloadCount)
	defer mockTG.Close()

	engine, client, stateDir := createEngineForTest(t, mockTG.URL, passphrase)

	var chunks []snapshot.ChunkRef
	for i := 0; i < chunkCount; i++ {
		chunkStart := i * chunkSize
		chunkEnd := chunkStart + chunkSize
		chunkPlain := allData[chunkStart:chunkEnd]
		fileID := fmt.Sprintf("mixed_env_%d", i)

		var encChunk []byte
		var err error

		if i%2 == 0 {
			// Even index: TGC1 (legacy uncompressed AES-256-GCM)
			salt := make([]byte, crypto.SaltSize)
			nonce := make([]byte, crypto.NonceSize)
			_, _ = rand.Read(salt)
			_, _ = rand.Read(nonce)
			key := crypto.DeriveKey(passphrase, salt)
			block, _ := aes.NewCipher(key)
			gcm, _ := cipher.NewGCM(block)
			ct := gcm.Seal(nil, nonce, chunkPlain, crypto.MagicHeaderV1)

			encChunk = append(encChunk, crypto.MagicHeaderV1...)
			encChunk = append(encChunk, salt...)
			encChunk = append(encChunk, nonce...)
			encChunk = append(encChunk, ct...)
		} else {
			// Odd index: TGC2 (modern Zstd compressed AES-256-GCM)
			encChunk, err = crypto.EncryptChunkWithCompression(chunkPlain, passphrase, crypto.CompressionZstd)
			if err != nil {
				t.Fatalf("encrypt TGC2 chunk %d failed: %v", i, err)
			}
		}

		mu.Lock()
		uploaded[fileID] = encChunk
		mu.Unlock()

		chunks = append(chunks, snapshot.ChunkRef{
			Index:          i,
			Offset:         int64(chunkStart),
			Size:           int64(chunkSize),
			EncryptedSize:  int64(len(encChunk)),
			Hash:           crypto.HashData(chunkPlain),
			TelegramFileID: fileID,
		})
	}

	manifest := &snapshot.SnapshotManifest{
		SnapshotID:  "snap-mixed-challenge",
		CreatedAt:   time.Now(),
		NodeName:    "Test-NAS",
		SourcePath:  "/source",
		TotalFiles:  1,
		TotalBytes:  int64(totalSize),
		Directories: []string{},
		Files: map[string]snapshot.FileEntry{
			"mixed_envelope.bin": {
				Path:     "mixed_envelope.bin",
				Size:     int64(totalSize),
				ModTime:  time.Now().Unix(),
				FileHash: crypto.HashData(allData),
				Chunks:   chunks,
			},
		},
		WORM: snapshot.WORMMetadata{
			Enabled:       true,
			Mode:          "compliance",
			LockedAt:      time.Now(),
			RetainUntil:   time.Now().Add(24 * time.Hour),
			RetentionDays: 1,
		},
	}
	manifest.WORM.ManifestHMAC = manifest.ComputeHMAC(passphrase)
	data, _ := json.MarshalIndent(manifest, "", "  ")
	_ = os.WriteFile(filepath.Join(stateDir, "snap-mixed-challenge.json"), data, 0644)

	cfg := ServerConfig{
		Addr:     "127.0.0.1",
		Port:     8089,
		ReadOnly: true,
	}
	srv, err := NewServer(cfg, engine, client, passphrase)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// 1. Full sequential streaming read via WebDAV GET
	req := httptest.NewRequest("GET", "/latest/mixed_envelope.bin", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("full GET failed: status %d", resp.StatusCode)
	}
	fullBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if len(fullBody) != totalSize {
		t.Fatalf("expected full body %d bytes, got %d", totalSize, len(fullBody))
	}
	if !bytes.Equal(fullBody, allData) {
		t.Fatalf("full streaming body mismatch with ground truth")
	}

	// 2. Cross-chunk read spanning from TGC1 into TGC2
	// Chunk 0 is TGC1 (ends at 20,000), Chunk 1 is TGC2 (starts at 20,000)
	reqCross1 := httptest.NewRequest("GET", "/latest/mixed_envelope.bin", nil)
	reqCross1.Header.Set("Range", "bytes=19980-20019") // 40 bytes: 20 from TGC1, 20 from TGC2
	wCross1 := httptest.NewRecorder()
	srv.ServeHTTP(wCross1, reqCross1)

	respCross1 := wCross1.Result()
	bodyCross1, _ := io.ReadAll(respCross1.Body)
	respCross1.Body.Close()

	if respCross1.StatusCode != http.StatusPartialContent {
		t.Fatalf("cross TGC1->TGC2 GET failed: status %d", respCross1.StatusCode)
	}
	if !bytes.Equal(bodyCross1, allData[19980:20020]) {
		t.Fatalf("cross TGC1->TGC2 payload mismatch")
	}

	// 3. Cross-chunk read spanning from TGC2 into TGC1
	// Chunk 1 is TGC2 (ends at 40,000), Chunk 2 is TGC1 (starts at 40,000)
	reqCross2 := httptest.NewRequest("GET", "/latest/mixed_envelope.bin", nil)
	reqCross2.Header.Set("Range", "bytes=39980-40019")
	wCross2 := httptest.NewRecorder()
	srv.ServeHTTP(wCross2, reqCross2)

	respCross2 := wCross2.Result()
	bodyCross2, _ := io.ReadAll(respCross2.Body)
	respCross2.Body.Close()

	if respCross2.StatusCode != http.StatusPartialContent {
		t.Fatalf("cross TGC2->TGC1 GET failed: status %d", respCross2.StatusCode)
	}
	if !bytes.Equal(bodyCross2, allData[39980:40020]) {
		t.Fatalf("cross TGC2->TGC1 payload mismatch")
	}
}

// =========================================================================
// TEST 4: High-Concurrency Range Requests & Single-Flight Coalescing
// =========================================================================

func TestChallenge_ConcurrentRangeRequests_SingleFlightCoalescing(t *testing.T) {
	passphrase := "webdav-challenge-passphrase-32b"
	const fileSize = 100000

	fileData := make([]byte, fileSize)
	for i := 0; i < fileSize; i++ {
		fileData[i] = byte((i * 19) % 256)
	}

	var downloadCount int64
	// 40ms artificial latency to ensure all concurrent requests overlap
	mockTG, uploaded, mu := setupMockTelegramWithLatency(t, 40*time.Millisecond, &downloadCount)
	defer mockTG.Close()

	engine, client, stateDir := createEngineForTest(t, mockTG.URL, passphrase)

	encChunk, err := crypto.EncryptChunkWithCompression(fileData, passphrase, crypto.CompressionZstd)
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}

	fileID := "coalesce_chunk_single"
	mu.Lock()
	uploaded[fileID] = encChunk
	mu.Unlock()

	manifest := &snapshot.SnapshotManifest{
		SnapshotID:  "snap-coalesce-test",
		CreatedAt:   time.Now(),
		NodeName:    "Test-NAS",
		SourcePath:  "/source",
		TotalFiles:  1,
		TotalBytes:  int64(fileSize),
		Directories: []string{},
		Files: map[string]snapshot.FileEntry{
			"concurrent_target.bin": {
				Path:     "concurrent_target.bin",
				Size:     int64(fileSize),
				ModTime:  time.Now().Unix(),
				FileHash: crypto.HashData(fileData),
				Chunks: []snapshot.ChunkRef{
					{
						Index:          0,
						Offset:         0,
						Size:           int64(fileSize),
						EncryptedSize:  int64(len(encChunk)),
						Hash:           crypto.HashData(fileData),
						TelegramFileID: fileID,
					},
				},
			},
		},
		WORM: snapshot.WORMMetadata{
			Enabled:       true,
			Mode:          "compliance",
			LockedAt:      time.Now(),
			RetainUntil:   time.Now().Add(24 * time.Hour),
			RetentionDays: 1,
		},
	}
	manifest.WORM.ManifestHMAC = manifest.ComputeHMAC(passphrase)
	data, _ := json.MarshalIndent(manifest, "", "  ")
	_ = os.WriteFile(filepath.Join(stateDir, "snap-coalesce-test.json"), data, 0644)

	cfg := ServerConfig{
		Addr:     "127.0.0.1",
		Port:     8089,
		ReadOnly: true,
	}
	srv, err := NewServer(cfg, engine, client, passphrase)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// 50 concurrent callers requesting different byte ranges within the same chunk
	const concurrency = 50
	var wg sync.WaitGroup
	errorsChan := make(chan error, concurrency)

	startBarrier := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			// Wait for synchronized start
			<-startBarrier

			// Worker requests bytes [1000 + i*10 .. 1000 + i*10 + 50]
			start := int64(1000 + workerID*10)
			end := start + 50
			rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end)

			req := httptest.NewRequest("GET", "/latest/concurrent_target.bin", nil)
			req.Header.Set("Range", rangeHeader)
			w := httptest.NewRecorder()

			srv.ServeHTTP(w, req)
			resp := w.Result()
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != http.StatusPartialContent {
				errorsChan <- fmt.Errorf("worker %d: expected 206, got %d", workerID, resp.StatusCode)
				return
			}

			expectedSlice := fileData[start : end+1]
			if !bytes.Equal(body, expectedSlice) {
				errorsChan <- fmt.Errorf("worker %d: data mismatch", workerID)
				return
			}
		}(i)
	}

	// Release all workers simultaneously
	close(startBarrier)
	wg.Wait()
	close(errorsChan)

	for err := range errorsChan {
		t.Errorf("concurrency error: %v", err)
	}

	// VERIFY SINGLE-FLIGHT COALESCING:
	// Exactly 1 download request should have been made to the Telegram server!
	actualDownloads := atomic.LoadInt64(&downloadCount)
	if actualDownloads != 1 {
		t.Fatalf("SINGLE-FLIGHT VIOLATION: expected exactly 1 chunk download from Telegram API, but observed %d downloads across %d concurrent Range requests!", actualDownloads, concurrency)
	}
}

// =========================================================================
// TEST 5: High Concurrency Multi-Chunk Cache Contention (-race stress)
// =========================================================================

func TestChallenge_HighConcurrency_CacheContention(t *testing.T) {
	cache := NewChunkCache(3, 60*time.Second) // Limited capacity: 3 chunks
	defer cache.Close()

	const chunkCount = 8
	const chunkSize = 10000
	testChunks := make([][]byte, chunkCount)
	for i := 0; i < chunkCount; i++ {
		testChunks[i] = make([]byte, chunkSize)
		for j := 0; j < chunkSize; j++ {
			testChunks[i][j] = byte(i*17 + j)
		}
	}

	// 60 concurrent workers randomly fetching and evicting across 8 chunks
	const workers = 60
	const iterations = 50
	var wg sync.WaitGroup
	errCount := int64(0)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				chunkIdx := (workerID + it) % chunkCount
				key := fmt.Sprintf("content_chunk_%d", chunkIdx)

				data, err := cache.GetOrFetch(context.Background(), key, func() ([]byte, error) {
					// Simulate small latency
					time.Sleep(2 * time.Millisecond)
					return testChunks[chunkIdx], nil
				})

				if err != nil {
					atomic.AddInt64(&errCount, 1)
					return
				}
				if !bytes.Equal(data, testChunks[chunkIdx]) {
					atomic.AddInt64(&errCount, 1)
					return
				}
			}
		}(w)
	}

	wg.Wait()
	if errCount > 0 {
		t.Fatalf("observed %d errors during high-concurrency cache contention", errCount)
	}
}

// =========================================================================
// TEST 6: Cache Capacity = 1 Chunk & Single Read Spanning Multiple Chunks
// =========================================================================

func TestChallenge_CacheCapacityOne_CrossChunkRead(t *testing.T) {
	passphrase := "webdav-challenge-passphrase-32b"
	mockTG, uploaded, mu := setupMockTelegramWithLatency(t, 0, nil)
	defer mockTG.Close()

	_, client, _ := createEngineForTest(t, mockTG.URL, passphrase)

	// Capacity is 1 chunk: cache MUST evict while processing a single multi-chunk read!
	cache := NewChunkCache(1, 60*time.Second)
	defer cache.Close()

	c0 := []byte("FIRST_CHUNK_DATA_HERE_")
	c1 := []byte("SECOND_CHUNK_DATA_HERE_")
	c2 := []byte("THIRD_CHUNK_DATA_HERE_")
	totalPlain := append(append(c0, c1...), c2...)

	// Encrypt all 3 chunks into mock Telegram
	enc0, _ := crypto.EncryptChunkWithCompression(c0, passphrase, crypto.CompressionZstd)
	enc1, _ := crypto.EncryptChunkWithCompression(c1, passphrase, crypto.CompressionZstd)
	enc2, _ := crypto.EncryptChunkWithCompression(c2, passphrase, crypto.CompressionZstd)

	mu.Lock()
	uploaded["cap1_c0"] = enc0
	uploaded["cap1_c1"] = enc1
	uploaded["cap1_c2"] = enc2
	mu.Unlock()

	fe := snapshot.FileEntry{
		Path: "cap1_file.bin",
		Size: int64(len(totalPlain)),
		Chunks: []snapshot.ChunkRef{
			{Index: 0, Offset: 0, Size: int64(len(c0)), TelegramFileID: "cap1_c0"},
			{Index: 1, Offset: int64(len(c0)), Size: int64(len(c1)), TelegramFileID: "cap1_c1"},
			{Index: 2, Offset: int64(len(c0) + len(c1)), Size: int64(len(c2)), TelegramFileID: "cap1_c2"},
		},
	}

	f := newWebdavRegularFile(context.Background(), "cap1_file.bin", fe, client, cache, passphrase)
	defer f.Close()

	buf := make([]byte, len(totalPlain))
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read with capacity 1 failed: %v", err)
	}
	if n != len(totalPlain) {
		t.Fatalf("expected %d bytes, got %d", len(totalPlain), n)
	}
	if !bytes.Equal(buf, totalPlain) {
		t.Fatalf("data mismatch with capacity 1 cache")
	}
}

// =========================================================================
// TEST 7: Closed File & Directory Method Integrity
// =========================================================================

func TestChallenge_ClosedFileAndDirOperations(t *testing.T) {
	passphrase := "webdav-challenge-passphrase-32b"
	content := []byte("Sample content for closed test")

	cache := NewChunkCache(2, 60*time.Second)
	defer cache.Close()
	cache.Put("closed_c0", content)

	fe := snapshot.FileEntry{
		Path: "closed_file.txt",
		Size: int64(len(content)),
		Chunks: []snapshot.ChunkRef{
			{Index: 0, Offset: 0, Size: int64(len(content)), TelegramFileID: "closed_c0"},
		},
	}

	f := newWebdavRegularFile(context.Background(), "closed_file.txt", fe, nil, cache, passphrase)

	// Close the file
	_ = f.Close()

	// 1. Read on closed file must return os.ErrClosed
	buf := make([]byte, 10)
	_, err := f.Read(buf)
	if err != os.ErrClosed {
		t.Errorf("expected os.ErrClosed on Read, got: %v", err)
	}

	// 2. Seek on closed file must return os.ErrClosed
	_, err = f.Seek(0, io.SeekStart)
	if err != os.ErrClosed {
		t.Errorf("expected os.ErrClosed on Seek, got: %v", err)
	}

	// 3. Stat on closed file must return os.ErrClosed
	_, err = f.Stat()
	if err != os.ErrClosed {
		t.Errorf("expected os.ErrClosed on Stat, got: %v", err)
	}

	// 4. Directory Seek & Readdir tests
	dirEntries := []os.FileInfo{
		&virtualFileInfo{name: "f1.txt", size: 10, mode: 0644, modTime: time.Now()},
		&virtualFileInfo{name: "f2.txt", size: 20, mode: 0644, modTime: time.Now()},
	}
	dir := newWebdavDir("mydir", time.Now(), dirEntries)

	// Readdir count=1
	entries1, err := dir.Readdir(1)
	if err != nil || len(entries1) != 1 || entries1[0].Name() != "f1.txt" {
		t.Errorf("readdir 1 failed: %v", err)
	}

	// Seek to non-zero must fail
	_, err = dir.Seek(5, io.SeekStart)
	if err == nil {
		t.Errorf("expected error for non-zero seek on dir")
	}

	// Seek to 0 must succeed and reset cursor
	pos, err := dir.Seek(0, io.SeekStart)
	if err != nil || pos != 0 {
		t.Errorf("reset seek on dir failed: %v", err)
	}

	// Readdir all remaining
	entriesAll, err := dir.Readdir(0)
	if err != nil || len(entriesAll) != 2 {
		t.Errorf("expected 2 entries after reset, got %d (err: %v)", len(entriesAll), err)
	}

	// Close dir
	_ = dir.Close()
	_, err = dir.Stat()
	if err != os.ErrClosed {
		t.Errorf("expected os.ErrClosed on closed dir Stat, got: %v", err)
	}
}

// =========================================================================
// TEST 8: Context Cancellation during Single-Flight Wait
// =========================================================================

func TestChallenge_ContextCancellation_SingleFlight(t *testing.T) {
	cache := NewChunkCache(4, 60*time.Second)
	defer cache.Close()

	leaderStarted := make(chan struct{})
	leaderCanFinish := make(chan struct{})

	// Goroutine 1: Leader with slow fetch
	go func() {
		_, _ = cache.GetOrFetch(context.Background(), "cancel_chunk", func() ([]byte, error) {
			close(leaderStarted)
			<-leaderCanFinish
			return []byte("leader finished data"), nil
		})
	}()

	<-leaderStarted

	// Goroutine 2: Follower with already-cancelled or timed-out context
	ctxTimeout, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	data, err := cache.GetOrFetch(ctxTimeout, "cancel_chunk", func() ([]byte, error) {
		t.Errorf("follower fetch() should never be invoked!")
		return nil, nil
	})

	if err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded for follower, got: %v", err)
	}
	if data != nil {
		t.Errorf("expected nil data for cancelled follower")
	}

	// Allow leader to finish
	close(leaderCanFinish)

	// Goroutine 3: After leader finishes, subsequent caller gets data from cache cleanly
	time.Sleep(50 * time.Millisecond)
	cachedData, found := cache.Get("cancel_chunk")
	if !found || string(cachedData) != "leader finished data" {
		t.Errorf("expected leader data to be safely cached, found: %v, val: %s", found, string(cachedData))
	}
}

// =========================================================================
// TEST 9: Checksum Mismatch Detection and Error Handling
// =========================================================================

func TestChallenge_ChecksumMismatch_GracefulError(t *testing.T) {
	passphrase := "webdav-challenge-passphrase-32b"
	mockTG, uploaded, mu := setupMockTelegramWithLatency(t, 0, nil)
	defer mockTG.Close()

	_, client, _ := createEngineForTest(t, mockTG.URL, passphrase)
	cache := NewChunkCache(4, 60*time.Second)
	defer cache.Close()

	plainContent := []byte("Original chunk content before tampering")
	encChunk, _ := crypto.EncryptChunkWithCompression(plainContent, passphrase, crypto.CompressionZstd)

	mu.Lock()
	uploaded["tampered_chunk_0"] = encChunk
	mu.Unlock()

	// Deliberately put WRONG expected hash in ChunkRef
	fe := snapshot.FileEntry{
		Path: "tampered.bin",
		Size: int64(len(plainContent)),
		Chunks: []snapshot.ChunkRef{
			{
				Index:          0,
				Offset:         0,
				Size:           int64(len(plainContent)),
				Hash:           "0000000000000000000000000000000000000000000000000000000000000000", // Tampered hash
				TelegramFileID: "tampered_chunk_0",
			},
		},
	}

	f := newWebdavRegularFile(context.Background(), "tampered.bin", fe, client, cache, passphrase)
	defer f.Close()

	buf := make([]byte, len(plainContent))
	n, err := f.Read(buf)
	if err == nil {
		t.Fatalf("expected checksum mismatch error, got nil (read %d bytes)", n)
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected 'checksum mismatch' error message, got: %v", err)
	}
}

// =========================================================================
// TEST 10: Decryption Failure with Wrong Passphrase
// =========================================================================

func TestChallenge_DecryptionFailure_WrongPassphrase(t *testing.T) {
	correctPass := "correct-secret-passphrase-32b"
	wrongPass := "wrong-secret-passphrase-32b"
	mockTG, uploaded, mu := setupMockTelegramWithLatency(t, 0, nil)
	defer mockTG.Close()

	_, client, _ := createEngineForTest(t, mockTG.URL, correctPass)
	cache := NewChunkCache(4, 60*time.Second)
	defer cache.Close()

	plainContent := []byte("Encrypted data that cannot be decrypted with wrong pass")
	encChunk, _ := crypto.EncryptChunkWithCompression(plainContent, correctPass, crypto.CompressionZstd)

	mu.Lock()
	uploaded["wrong_pass_chunk"] = encChunk
	mu.Unlock()

	fe := snapshot.FileEntry{
		Path: "secret.bin",
		Size: int64(len(plainContent)),
		Chunks: []snapshot.ChunkRef{
			{
				Index:          0,
				Offset:         0,
				Size:           int64(len(plainContent)),
				Hash:           crypto.HashData(plainContent),
				TelegramFileID: "wrong_pass_chunk",
			},
		},
	}

	// Initialize file with wrong passphrase
	f := newWebdavRegularFile(context.Background(), "secret.bin", fe, client, cache, wrongPass)
	defer f.Close()

	buf := make([]byte, len(plainContent))
	_, err := f.Read(buf)
	if err == nil {
		t.Fatalf("expected decryption error with wrong passphrase, got nil")
	}
	if !strings.Contains(err.Error(), "decrypt chunk") {
		t.Fatalf("expected decryption error, got: %v", err)
	}
}
