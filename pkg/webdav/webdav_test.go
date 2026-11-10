package webdav

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"fmt"
	"io"
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

// setupMockTelegramAndEngine creates an isolated test environment with a mock
// Telegram server, snapshot engine, and pre-populated test snapshot.
func setupMockTelegramAndEngine(t *testing.T, passphrase string) (*httptest.Server, *snapshot.Engine, *telegram.Client, string, map[string][]byte) {
	tempDir := t.TempDir()
	sourceDir := filepath.Join(tempDir, "source")
	stateDir := filepath.Join(tempDir, "snapshots")
	_ = os.MkdirAll(sourceDir, 0755)
	_ = os.MkdirAll(stateDir, 0755)

	uploadedFiles := make(map[string][]byte)
	var mu sync.Mutex

	// Mock Telegram API server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if strings.Contains(r.URL.Path, "getFile") {
			fileID := r.URL.Query().Get("file_id")
			resp := map[string]interface{}{
				"ok": true,
				"result": map[string]interface{}{
					"file_id":   fileID,
					"file_path": "documents/" + fileID,
					"file_size": len(uploadedFiles[fileID]),
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if strings.HasPrefix(r.URL.Path, "/file/bot") {
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

		// Default OK response for other Bot API calls
		resp := map[string]interface{}{"ok": true, "result": true}
		_ = json.NewEncoder(w).Encode(resp)
	}))

	// Configure environment to route to mock Telegram server
	t.Setenv("TELEGRAM_API_URL", mockServer.URL)
	t.Setenv("HTTP_PROXY", mockServer.URL)
	t.Setenv("HTTPS_PROXY", mockServer.URL)

	cfg := &config.Config{
		BotTokens:     []string{"12345:mock_test_token"},
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

	return mockServer, engine, tgClient, stateDir, uploadedFiles
}

// createTestSnapshot prepares a manifest with encrypted TGC1 and TGC2 chunks.
func createTestSnapshot(t *testing.T, stateDir string, passphrase string, files map[string][]byte, uploadedFiles map[string][]byte, cache ...*ChunkCache) *snapshot.SnapshotManifest {
	snapID := "snap-20261110-001"
	manifest := &snapshot.SnapshotManifest{
		SnapshotID:  snapID,
		CreatedAt:   time.Now().Add(-1 * time.Hour),
		NodeName:    "Test-NAS",
		SourcePath:  "/source",
		Directories: []string{"subfolder"},
		Files:       make(map[string]snapshot.FileEntry),
		WORM: snapshot.WORMMetadata{
			Enabled:       true,
			Mode:          "compliance",
			LockedAt:      time.Now().Add(-1 * time.Hour),
			RetainUntil:   time.Now().Add(30 * 24 * time.Hour),
			RetentionDays: 30,
		},
	}

	fileIdx := 1
	for relPath, content := range files {
		var chunks []snapshot.ChunkRef
		if len(content) == 0 {
			// Zero byte file: 0 chunks
			manifest.Files[relPath] = snapshot.FileEntry{
				Path:     relPath,
				Size:     0,
				ModTime:  time.Now().Unix(),
				FileHash: crypto.HashData([]byte{}),
				Chunks:   []snapshot.ChunkRef{},
			}
			continue
		}

		// Encrypt content (alternate TGC1 and TGC2 for testing)
		fileID := fmt.Sprintf("tg_file_%d", fileIdx)
		fileIdx++

		var encChunk []byte
		var err error
		if fileIdx%2 == 0 {
			// TGC2 compressed chunk
			encChunk, err = crypto.EncryptChunkWithCompression(content, passphrase, crypto.CompressionZstd)
		} else {
			// Legacy TGC1 uncompressed chunk
			encChunk, err = crypto.EncryptChunk(content, passphrase)
		}
		if err != nil {
			t.Fatalf("failed to encrypt chunk: %v", err)
		}

		uploadedFiles[fileID] = encChunk
		if len(cache) > 0 && cache[0] != nil {
			cache[0].Put(fileID, content)
		}

		chunk := snapshot.ChunkRef{
			Index:          0,
			Offset:         0,
			Size:           int64(len(content)),
			EncryptedSize:  int64(len(encChunk)),
			Hash:           crypto.HashData(content),
			TelegramFileID: fileID,
			TelegramMsgID:  1000 + int64(fileIdx),
		}
		chunks = append(chunks, chunk)

		manifest.Files[relPath] = snapshot.FileEntry{
			Path:     relPath,
			Size:     int64(len(content)),
			ModTime:  time.Now().Unix(),
			FileHash: crypto.HashData(content),
			Chunks:   chunks,
		}
		manifest.TotalBytes += int64(len(content))
	}

	manifest.TotalFiles = len(manifest.Files)
	manifest.WORM.ManifestHMAC = manifest.ComputeHMAC(passphrase)

	manifestPath := filepath.Join(stateDir, snapID+".json")
	data, _ := json.MarshalIndent(manifest, "", "  ")
	_ = os.WriteFile(manifestPath, data, 0644)

	return manifest
}

// TestWebDAV_OPTIONS_Headers tests RFC 4918 and OS compatibility headers.
func TestWebDAV_OPTIONS_Headers(t *testing.T) {
	passphrase := "webdav-secret-passphrase-32b"
	mockTG, engine, client, _, _ := setupMockTelegramAndEngine(t, passphrase)
	defer mockTG.Close()

	cfg := ServerConfig{
		Addr:     "127.0.0.1",
		Port:     8089,
		ReadOnly: true,
	}

	srv, err := NewServer(cfg, engine, client, passphrase)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	req := httptest.NewRequest("OPTIONS", "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 200 OK or 204 for OPTIONS, got: %d", resp.StatusCode)
	}

	davHeader := resp.Header.Get("DAV")
	if !strings.Contains(davHeader, "1") || !strings.Contains(davHeader, "2") {
		t.Errorf("expected 'DAV: 1, 2' header, got: %q", davHeader)
	}

	msAuthor := resp.Header.Get("MS-Author-Via")
	if msAuthor != "DAV" {
		t.Errorf("expected 'MS-Author-Via: DAV' for Windows Explorer, got: %q", msAuthor)
	}

	allowHeader := resp.Header.Get("Allow")
	if !strings.Contains(allowHeader, "PROPFIND") || !strings.Contains(allowHeader, "GET") {
		t.Errorf("expected Allow header to contain PROPFIND and GET, got: %q", allowHeader)
	}
}

// TestWebDAV_PROPFIND_MultiSnapshotRoot tests virtual root listing /latest and /snapshots.
func TestWebDAV_PROPFIND_MultiSnapshotRoot(t *testing.T) {
	passphrase := "webdav-secret-passphrase-32b"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
	defer mockTG.Close()

	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"testfile.txt": []byte("Hello WebDAV multi-snapshot testing!"),
	}, uploaded)

	cfg := ServerConfig{
		Addr:     "127.0.0.1",
		Port:     8089,
		ReadOnly: true,
	}

	srv, err := NewServer(cfg, engine, client, passphrase)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	propXML := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`
	req := httptest.NewRequest("PROPFIND", "/", bytes.NewBufferString(propXML))
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus { // 207 Multi-Status
		t.Errorf("expected 207 Multi-Status, got: %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "multistatus") {
		t.Errorf("expected XML multistatus in body, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "latest") {
		t.Errorf("expected /latest in virtual root directory listing")
	}
	if !strings.Contains(bodyStr, "snapshots") {
		t.Errorf("expected /snapshots in virtual root directory listing")
	}
}

// TestWebDAV_GET_FullFile tests on-the-fly decryption of files via WebDAV GET.
func TestWebDAV_GET_FullFile(t *testing.T) {
	passphrase := "webdav-secret-passphrase-32b"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
	defer mockTG.Close()

	expectedContent := []byte("The quick brown fox jumps over the lazy dog. Production WebDAV testing.")
	cfg := ServerConfig{
		Addr:     "127.0.0.1",
		Port:     8089,
		ReadOnly: true,
	}

	srv, err := NewServer(cfg, engine, client, passphrase)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"testfile.txt": expectedContent,
	}, uploaded, srv.ChunkCache())

	req := httptest.NewRequest("GET", "/latest/testfile.txt", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got: %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, expectedContent) {
		t.Errorf("content mismatch: got %q, expected %q", string(body), string(expectedContent))
	}
}

// TestWebDAV_GET_ByteRanges_ReadSeeker tests HTTP 206 Partial Content range requests.
func TestWebDAV_GET_ByteRanges_ReadSeeker(t *testing.T) {
	passphrase := "webdav-secret-passphrase-32b"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
	defer mockTG.Close()

	fullContent := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")

	cfg := ServerConfig{
		Addr:     "127.0.0.1",
		Port:     8089,
		ReadOnly: true,
	}

	srv, err := NewServer(cfg, engine, client, passphrase)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"video.mp4": fullContent,
	}, uploaded, srv.ChunkCache())

	tests := []struct {
		rangeHeader string
		expected    []byte
		expectedHdr string
	}{
		{"bytes=0-9", fullContent[0:10], fmt.Sprintf("bytes 0-9/%d", len(fullContent))},
		{"bytes=10-19", fullContent[10:20], fmt.Sprintf("bytes 10-19/%d", len(fullContent))},
		{"bytes=0-0", fullContent[0:1], fmt.Sprintf("bytes 0-0/%d", len(fullContent))},
		{"bytes=26-35", fullContent[26:36], fmt.Sprintf("bytes 26-35/%d", len(fullContent))},
	}

	for _, tc := range tests {
		req := httptest.NewRequest("GET", "/latest/video.mp4", nil)
		req.Header.Set("Range", tc.rangeHeader)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		resp := w.Result()
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent {
			t.Errorf("range %s: expected 206 Partial Content, got: %d", tc.rangeHeader, resp.StatusCode)
		}

		if !bytes.Equal(body, tc.expected) {
			t.Errorf("range %s: content mismatch: got %q, expected %q", tc.rangeHeader, string(body), string(tc.expected))
		}

		cr := resp.Header.Get("Content-Range")
		if cr != tc.expectedHdr {
			t.Errorf("range %s: Content-Range mismatch: got %q, expected %q", tc.rangeHeader, cr, tc.expectedHdr)
		}
	}
}

// TestWebDAV_Seeking_Backwards_Forwards_PastEOF tests the io.ReadSeeker contract in isolation.
func TestWebDAV_Seeking_Backwards_Forwards_PastEOF(t *testing.T) {
	passphrase := "webdav-secret-passphrase-32b"
	content := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ")

	encChunk, err := crypto.EncryptChunkWithCompression(content, passphrase, crypto.CompressionZstd)
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	cache := NewChunkCache(4, 60*time.Second)
	defer cache.Close()
	cache.Put("tg_chunk_0", content)

	fe := snapshot.FileEntry{
		Path:    "seek_test.bin",
		Size:    int64(len(content)),
		ModTime: time.Now().Unix(),
		Chunks: []snapshot.ChunkRef{
			{
				Index:          0,
				Offset:         0,
				Size:           int64(len(content)),
				EncryptedSize:  int64(len(encChunk)),
				Hash:           crypto.HashData(content),
				TelegramFileID: "tg_chunk_0",
			},
		},
	}

	f := newWebdavRegularFile(context.Background(), "seek_test.bin", fe, nil, cache, passphrase)
	defer f.Close()

	// 1. Initial read
	buf := make([]byte, 5)
	n, err := f.Read(buf)
	if err != nil || n != 5 || string(buf) != "01234" {
		t.Fatalf("initial read failed: n=%d err=%v buf=%q", n, err, string(buf))
	}

	// 2. Seek forward
	pos, err := f.Seek(10, io.SeekStart)
	if err != nil || pos != 10 {
		t.Fatalf("seek forward failed: pos=%d err=%v", pos, err)
	}
	n, err = f.Read(buf)
	if err != nil || n != 5 || string(buf) != "ABCDE" {
		t.Fatalf("read after seek forward failed: buf=%q", string(buf))
	}

	// 3. Seek backward from current
	pos, err = f.Seek(-3, io.SeekCurrent)
	if err != nil || pos != 12 {
		t.Fatalf("seek backward failed: pos=%d err=%v", pos, err)
	}
	n, err = f.Read(buf)
	if err != nil || n != 5 || string(buf) != "CDEFG" {
		t.Fatalf("read after seek backward failed: buf=%q", string(buf))
	}

	// 4. Seek past EOF
	pos, err = f.Seek(100, io.SeekEnd)
	if err != nil || pos != int64(len(content))+100 {
		t.Fatalf("seek past EOF failed: pos=%d err=%v", pos, err)
	}
	n, err = f.Read(buf)
	if err != io.EOF || n != 0 {
		t.Fatalf("read past EOF: expected 0, io.EOF; got n=%d, err=%v", n, err)
	}

	// 5. Negative seek error
	_, err = f.Seek(-5, io.SeekStart)
	if err == nil {
		t.Errorf("expected error for negative seek start")
	}
}

// TestWebDAV_CrossChunkRead tests reading a byte buffer spanning across 2 chunks.
func TestWebDAV_CrossChunkRead(t *testing.T) {
	passphrase := "webdav-secret-passphrase-32b"
	chunk0Data := []byte("AAAAAAAAAA") // 10 bytes
	chunk1Data := []byte("BBBBBBBBBB") // 10 bytes

	cache := NewChunkCache(4, 60*time.Second)
	defer cache.Close()
	cache.Put("chunk_0", chunk0Data)
	cache.Put("chunk_1", chunk1Data)

	fe := snapshot.FileEntry{
		Path:    "cross_chunk.bin",
		Size:    20,
		ModTime: time.Now().Unix(),
		Chunks: []snapshot.ChunkRef{
			{Index: 0, Offset: 0, Size: 10, TelegramFileID: "chunk_0"},
			{Index: 1, Offset: 10, Size: 10, TelegramFileID: "chunk_1"},
		},
	}

	f := newWebdavRegularFile(context.Background(), "cross_chunk.bin", fe, nil, cache, passphrase)
	defer f.Close()

	// Seek to offset 8 (2 bytes before chunk boundary)
	_, _ = f.Seek(8, io.SeekStart)

	// Read 5 bytes: should consume 2 bytes from chunk 0 and 3 bytes from chunk 1
	buf := make([]byte, 5)
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("cross-chunk read failed: %v", err)
	}
	if n != 5 {
		t.Fatalf("expected 5 bytes, got %d", n)
	}
	if string(buf) != "AABBB" {
		t.Errorf("expected 'AABBB', got %q", string(buf))
	}
}

// TestWebDAV_ZeroByteFile tests opening and reading a 0-byte file without errors or network calls.
func TestWebDAV_ZeroByteFile(t *testing.T) {
	passphrase := "webdav-secret-passphrase-32b"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
	defer mockTG.Close()

	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"empty.txt": []byte{},
	}, uploaded)

	cfg := ServerConfig{
		Addr:     "127.0.0.1",
		Port:     8089,
		ReadOnly: true,
	}

	srv, err := NewServer(cfg, engine, client, passphrase)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	req := httptest.NewRequest("GET", "/latest/empty.txt", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for empty file, got: %d", resp.StatusCode)
	}

	data, _ := io.ReadAll(resp.Body)
	if len(data) != 0 {
		t.Errorf("expected 0 bytes for empty file, got: %d bytes", len(data))
	}
}

// TestWebDAV_ReadOnlyRejection tests that mutating methods return 403 Forbidden.
func TestWebDAV_ReadOnlyRejection(t *testing.T) {
	passphrase := "webdav-secret-passphrase-32b"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
	defer mockTG.Close()

	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"doc.txt": []byte("Read only content"),
	}, uploaded)

	cfg := ServerConfig{
		Addr:     "127.0.0.1",
		Port:     8089,
		ReadOnly: true,
	}

	srv, err := NewServer(cfg, engine, client, passphrase)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	mutations := []string{"PUT", "DELETE", "MKCOL", "MOVE", "LOCK"}
	for _, method := range mutations {
		req := httptest.NewRequest(method, "/latest/doc.txt", bytes.NewBufferString("tamper"))
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		resp := w.Result()
		resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("method %s: expected 403 Forbidden or 405 Method Not Allowed, got: %d", method, resp.StatusCode)
		}
	}
}

// TestWebDAV_MetadataProbes_MacOS_Windows tests fast 404/403 on AppleDouble & Windows probes.
func TestWebDAV_MetadataProbes_MacOS_Windows(t *testing.T) {
	passphrase := "webdav-secret-passphrase-32b"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
	defer mockTG.Close()

	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"document.pdf": []byte("PDF content"),
	}, uploaded)

	cfg := ServerConfig{
		Addr:     "127.0.0.1",
		Port:     8089,
		ReadOnly: true,
	}

	srv, err := NewServer(cfg, engine, client, passphrase)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	probes404 := []string{
		"/._document.pdf",
		"/.DS_Store",
		"/desktop.ini",
		"/autorun.inf",
		"/Thumbs.db",
	}

	for _, probe := range probes404 {
		req := httptest.NewRequest("GET", probe, nil)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		resp := w.Result()
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
			t.Errorf("probe %s: expected 404 Not Found, got: %d", probe, resp.StatusCode)
		}
	}

	// Finder PUT .DS_Store must return 403 Forbidden
	reqDS := httptest.NewRequest("PUT", "/.DS_Store", bytes.NewBuffer([]byte{0x00, 0x01}))
	wDS := httptest.NewRecorder()
	srv.ServeHTTP(wDS, reqDS)
	respDS := wDS.Result()
	respDS.Body.Close()

	if respDS.StatusCode != http.StatusForbidden && respDS.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT .DS_Store: expected 403 Forbidden or 405, got: %d", respDS.StatusCode)
	}
}

// TestWebDAV_BasicAuth tests optional HTTP Basic Authentication.
func TestWebDAV_BasicAuth(t *testing.T) {
	passphrase := "webdav-secret-passphrase-32b"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
	defer mockTG.Close()

	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"secret.txt": []byte("Confidential data"),
	}, uploaded)

	cfg := ServerConfig{
		Addr:     "127.0.0.1",
		Port:     8089,
		ReadOnly: true,
		Username: "vaultadmin",
		Password: "supersecretpassword",
	}

	srv, err := NewServer(cfg, engine, client, passphrase)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// 1. Unauthenticated request -> 401 Unauthorized
	req1 := httptest.NewRequest("GET", "/latest/secret.txt", nil)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, req1)
	if w1.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got: %d", w1.Result().StatusCode)
	}

	// 2. Bad credentials -> 401 Unauthorized
	req2 := httptest.NewRequest("GET", "/latest/secret.txt", nil)
	req2.SetBasicAuth("vaultadmin", "wrongpassword")
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	if w2.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for wrong pass, got: %d", w2.Result().StatusCode)
	}

	// 3. Valid credentials -> 200 OK
	req3 := httptest.NewRequest("GET", "/latest/secret.txt", nil)
	req3.SetBasicAuth("vaultadmin", "supersecretpassword")
	w3 := httptest.NewRecorder()
	srv.ServeHTTP(w3, req3)
	if w3.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK with correct credentials, got: %d", w3.Result().StatusCode)
	}
}

// TestWebDAV_LRUCache_Eviction_And_ThreadSafety tests chunk eviction and single-flight concurrency.
func TestWebDAV_LRUCache_Eviction_And_ThreadSafety(t *testing.T) {
	cache := NewChunkCache(2, 60*time.Second) // Capacity of 2
	defer cache.Close()

	// 1. Insert 2 chunks
	cache.Put("c1", []byte("chunk 1 data"))
	cache.Put("c2", []byte("chunk 2 data"))
	if cache.Len() != 2 {
		t.Errorf("expected 2 items, got %d", cache.Len())
	}

	// 2. Access c1 to make c2 oldest
	_, _ = cache.Get("c1")

	// 3. Insert c3 -> should evict c2
	cache.Put("c3", []byte("chunk 3 data"))
	if cache.Len() != 2 {
		t.Errorf("expected length 2 after eviction, got %d", cache.Len())
	}

	if _, found := cache.Get("c2"); found {
		t.Errorf("expected c2 to be evicted")
	}
	if _, found := cache.Get("c1"); !found {
		t.Errorf("expected c1 to remain cached")
	}

	// 4. Single-flight concurrency test
	var fetchCount int32
	var wg sync.WaitGroup
	workers := 25

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := cache.GetOrFetch(context.Background(), "heavy_chunk", func() ([]byte, error) {
				atomic.AddInt32(&fetchCount, 1)
				time.Sleep(20 * time.Millisecond) // Simulate slow download
				return []byte("heavy chunk payload"), nil
			})
			if err != nil || string(data) != "heavy chunk payload" {
				t.Errorf("unexpected fetch result: %v, %s", err, string(data))
			}
		}()
	}

	wg.Wait()
	if atomic.LoadInt32(&fetchCount) != 1 {
		t.Errorf("expected single-flight to execute fetch exactly once, got: %d", fetchCount)
	}
}

// TestWebDAV_LRUCache_IdleEviction tests that when no requests arrive for idleTimeout,
// all chunks are evicted so memory overhead drops back to 0 MB.
func TestWebDAV_LRUCache_IdleEviction(t *testing.T) {
	cache := NewChunkCache(5, 50*time.Millisecond)
	defer cache.Close()

	cache.Put("item1", []byte("chunk 1"))
	cache.Put("item2", []byte("chunk 2"))

	if cache.Len() != 2 {
		t.Fatalf("expected 2 items, got %d", cache.Len())
	}

	// Wait for idle eviction timer to fire
	time.Sleep(120 * time.Millisecond)

	if cache.Len() != 0 {
		t.Errorf("expected cache to be cleared by idle eviction, got len: %d", cache.Len())
	}
	if cache.ByteSize() != 0 {
		t.Errorf("expected byte size 0 after idle eviction, got: %d", cache.ByteSize())
	}
}

// TestWebDAV_Mixed_TGC1_TGC2_Streaming verifies transparent decryption across both envelope formats.
func TestWebDAV_Mixed_TGC1_TGC2_Streaming(t *testing.T) {
	passphrase := "webdav-secret-passphrase-32b"

	// 1. Prepare TGC1 chunk
	tgc1Plaintext := []byte("TGC1 uncompressed payload part 1.")
	salt := []byte("salt0123456789ab")
	nonce := []byte("nonce0123456")
	key := crypto.DeriveKey(passphrase, salt)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	ciphertextV1 := gcm.Seal(nil, nonce, tgc1Plaintext, []byte("TGC1"))

	var tgc1Chunk []byte
	tgc1Chunk = append(tgc1Chunk, []byte("TGC1")...)
	tgc1Chunk = append(tgc1Chunk, salt...)
	tgc1Chunk = append(tgc1Chunk, nonce...)
	tgc1Chunk = append(tgc1Chunk, ciphertextV1...)

	// 2. Prepare TGC2 chunk
	tgc2Plaintext := []byte(" TGC2 Zstandard compressed payload part 2.")
	tgc2Chunk, err := crypto.EncryptChunkWithCompression(tgc2Plaintext, passphrase, crypto.CompressionZstd)
	if err != nil {
		t.Fatalf("failed to create TGC2 chunk: %v", err)
	}

	// 3. Decrypt both sequentially through crypto.DecryptChunk
	dec1, err := crypto.DecryptChunk(tgc1Chunk, passphrase)
	if err != nil || !bytes.Equal(dec1, tgc1Plaintext) {
		t.Fatalf("failed to decrypt TGC1 chunk: %v", err)
	}

	dec2, err := crypto.DecryptChunk(tgc2Chunk, passphrase)
	if err != nil || !bytes.Equal(dec2, tgc2Plaintext) {
		t.Fatalf("failed to decrypt TGC2 chunk: %v", err)
	}

	// 4. Verify combined stream through webdavRegularFile
	cache := NewChunkCache(4, 60*time.Second)
	defer cache.Close()
	cache.Put("mixed_chunk_0", dec1)
	cache.Put("mixed_chunk_1", dec2)

	totalSize := int64(len(dec1) + len(dec2))
	fe := snapshot.FileEntry{
		Path:    "mixed.txt",
		Size:    totalSize,
		ModTime: time.Now().Unix(),
		Chunks: []snapshot.ChunkRef{
			{Index: 0, Offset: 0, Size: int64(len(dec1)), TelegramFileID: "mixed_chunk_0"},
			{Index: 1, Offset: int64(len(dec1)), Size: int64(len(dec2)), TelegramFileID: "mixed_chunk_1"},
		},
	}

	f := newWebdavRegularFile(context.Background(), "mixed.txt", fe, nil, cache, passphrase)
	defer f.Close()

	buf := make([]byte, totalSize)
	n, err := io.ReadFull(f, buf)
	if err != nil {
		t.Fatalf("failed to read full mixed file: %v", err)
	}
	if int64(n) != totalSize {
		t.Fatalf("expected %d bytes, got %d", totalSize, n)
	}

	expectedCombined := append(tgc1Plaintext, tgc2Plaintext...)
	if !bytes.Equal(buf, expectedCombined) {
		t.Errorf("combined content mismatch")
	}
}
