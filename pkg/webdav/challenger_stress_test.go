package webdav

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/snapshot"
)

// TestChallenger_LRU_Eviction_Order_And_Capacity rigorously tests LRU eviction ordering,
// capacity constraints, and byte accounting.
func TestChallenger_LRU_Eviction_Order_And_Capacity(t *testing.T) {
	cache := NewChunkCache(3, 10*time.Minute)
	defer cache.Close()

	// 1. Fill cache to capacity (3 items)
	cache.Put("c1", []byte("1111111111")) // 10 bytes
	cache.Put("c2", []byte("2222222222")) // 10 bytes
	cache.Put("c3", []byte("3333333333")) // 10 bytes

	if cache.Len() != 3 {
		t.Fatalf("expected 3 items, got %d", cache.Len())
	}
	if cache.ByteSize() != 30 {
		t.Fatalf("expected 30 bytes, got %d", cache.ByteSize())
	}

	// 2. Access c1: order from MRU to LRU becomes [c1, c3, c2]
	val, ok := cache.Get("c1")
	if !ok || string(val) != "1111111111" {
		t.Fatalf("failed to retrieve c1")
	}

	// 3. Put c4: should evict c2 (the least recently used)
	cache.Put("c4", []byte("4444444444")) // 10 bytes
	if cache.Len() != 3 {
		t.Fatalf("expected 3 items after eviction, got %d", cache.Len())
	}
	if cache.ByteSize() != 30 {
		t.Fatalf("expected 30 bytes after eviction, got %d", cache.ByteSize())
	}

	if _, found := cache.Get("c2"); found {
		t.Errorf("expected c2 to be evicted, but it was found")
	}
	if _, found := cache.Get("c1"); !found {
		t.Errorf("expected c1 to still be cached")
	}
	if _, found := cache.Get("c3"); !found {
		t.Errorf("expected c3 to still be cached")
	}
	if _, found := cache.Get("c4"); !found {
		t.Errorf("expected c4 to be cached")
	}

	// 4. Update c3 in place with a larger payload (30 bytes instead of 10 bytes)
	largerData := []byte("333333333333333333333333333333") // 30 bytes
	cache.Put("c3", largerData)

	if cache.Len() != 3 {
		t.Fatalf("expected length to remain 3 after in-place update, got %d", cache.Len())
	}
	expectedBytes := int64(10 + 10 + 30) // c1(10) + c4(10) + c3(30) = 50
	if cache.ByteSize() != expectedBytes {
		t.Fatalf("expected ByteSize to be %d, got %d", expectedBytes, cache.ByteSize())
	}

	// Order is now MRU: c3, c4, c1 :LRU
	// Put c5 -> should evict c1
	cache.Put("c5", []byte("5555555555"))
	if _, found := cache.Get("c1"); found {
		t.Errorf("expected c1 to be evicted, but it was found")
	}
	if _, found := cache.Get("c3"); !found {
		t.Errorf("expected c3 to still be cached")
	}
}

// TestChallenger_LRU_Idle_Eviction_And_RAM_Reclamation tests that:
// 1. When idleTimeout passes without activity, all chunks are evicted.
// 2. runtime.GC() and debug.FreeOSMemory() are triggered.
// 3. Active requests periodically reset the idle timer and prevent eviction.
func TestChallenger_LRU_Idle_Eviction_And_RAM_Reclamation(t *testing.T) {
	idleTimeout := 60 * time.Millisecond
	cache := NewChunkCache(5, idleTimeout)
	defer cache.Close()

	// Put 3 chunks of 1MB each
	chunk1MB := bytes.Repeat([]byte("A"), 1024*1024)
	cache.Put("big1", chunk1MB)
	cache.Put("big2", chunk1MB)
	cache.Put("big3", chunk1MB)

	if cache.Len() != 3 {
		t.Fatalf("expected 3 items, got %d", cache.Len())
	}
	if cache.ByteSize() != 3*1024*1024 {
		t.Fatalf("expected 3MB byte size, got %d", cache.ByteSize())
	}

	// Phase 1: Test keep-alive resets idle timer
	for i := 0; i < 4; i++ {
		time.Sleep(30 * time.Millisecond) // Sleep less than idleTimeout
		_, ok := cache.Get("big1")
		if !ok {
			t.Fatalf("iteration %d: cache evicted prematurely despite active Get requests", i)
		}
	}

	// Still alive because of periodic accesses
	if cache.Len() != 3 {
		t.Fatalf("expected cache to remain populated during active reads, got len %d", cache.Len())
	}

	// Phase 2: Now wait for idle timeout to fire without any access
	time.Sleep(150 * time.Millisecond)

	if cache.Len() != 0 {
		t.Errorf("expected 0 items after idle timeout, got %d", cache.Len())
	}
	if cache.ByteSize() != 0 {
		t.Errorf("expected 0 ByteSize after idle timeout, got %d", cache.ByteSize())
	}

	// Force explicit GC cycle check
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.Logf("HeapAlloc after idle eviction: %d KB", m.HeapAlloc/1024)
}

// TestChallenger_LRU_SingleFlight_Concurrency_And_ErrorResilience tests single-flight
// request deduplication, concurrent error propagation, and context cancellations.
func TestChallenger_LRU_SingleFlight_Concurrency_And_ErrorResilience(t *testing.T) {
	cache := NewChunkCache(10, 5*time.Minute)
	defer cache.Close()

	// 1. Fifty concurrent callers request the same chunk
	var fetchExecCount int32
	var wg sync.WaitGroup
	numCallers := 50
	expectedPayload := []byte("shared-chunk-payload-data")

	for i := 0; i < numCallers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := cache.GetOrFetch(context.Background(), "chunk_singleflight_1", func() ([]byte, error) {
				atomic.AddInt32(&fetchExecCount, 1)
				time.Sleep(30 * time.Millisecond) // Simulate slow network
				return expectedPayload, nil
			})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !bytes.Equal(data, expectedPayload) {
				t.Errorf("unexpected data returned: %q", string(data))
			}
		}()
	}

	wg.Wait()

	if atomic.LoadInt32(&fetchExecCount) != 1 {
		t.Fatalf("expected fetch to be executed exactly once, got %d", fetchExecCount)
	}

	// 2. Test single-flight with fetch ERROR: error must propagate to all callers,
	// error must NOT be cached, and subsequent retry must be able to succeed.
	var errFetchExecCount int32
	var wgErr sync.WaitGroup
	expectedErr := fmt.Errorf("mock telegram network timeout")

	for i := 0; i < 20; i++ {
		wgErr.Add(1)
		go func() {
			defer wgErr.Done()
			data, err := cache.GetOrFetch(context.Background(), "chunk_error_test", func() ([]byte, error) {
				atomic.AddInt32(&errFetchExecCount, 1)
				time.Sleep(20 * time.Millisecond)
				return nil, expectedErr
			})
			if err == nil || !strings.Contains(err.Error(), "network timeout") {
				t.Errorf("expected network timeout error, got err=%v data=%q", err, string(data))
			}
		}()
	}

	wgErr.Wait()

	if atomic.LoadInt32(&errFetchExecCount) != 1 {
		t.Fatalf("expected error fetch to be executed once across concurrent callers, got %d", errFetchExecCount)
	}

	// Cache must not contain the failed key
	if _, found := cache.Get("chunk_error_test"); found {
		t.Errorf("failed fetch must not be cached in items")
	}

	// 3. Retry the same key with success: must succeed now!
	retryData, err := cache.GetOrFetch(context.Background(), "chunk_error_test", func() ([]byte, error) {
		return []byte("retry succeeded"), nil
	})
	if err != nil || string(retryData) != "retry succeeded" {
		t.Fatalf("retry after error failed: err=%v data=%s", err, string(retryData))
	}

	// 4. Test context cancellation on waiting caller
	slowFetchStarted := make(chan struct{})
	slowFetchBlock := make(chan struct{})

	go func() {
		_, _ = cache.GetOrFetch(context.Background(), "slow_chunk", func() ([]byte, error) {
			close(slowFetchStarted)
			<-slowFetchBlock
			return []byte("slow data"), nil
		})
	}()

	<-slowFetchStarted // Leader is fetching

	// Second caller has a fast timeout
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = cache.GetOrFetch(timeoutCtx, "slow_chunk", func() ([]byte, error) {
		t.Fatalf("fetch should not be called by waiter")
		return nil, nil
	})
	if err != context.DeadlineExceeded {
		t.Fatalf("expected context.DeadlineExceeded for cancelled waiter, got: %v", err)
	}

	// Unblock leader
	close(slowFetchBlock)
	time.Sleep(30 * time.Millisecond)

	// Now third caller gets cached data
	cachedData, err := cache.GetOrFetch(context.Background(), "slow_chunk", func() ([]byte, error) {
		t.Fatalf("fetch should not be called on cache hit")
		return nil, nil
	})
	if err != nil || string(cachedData) != "slow data" {
		t.Fatalf("expected cached data, got err=%v data=%s", err, string(cachedData))
	}
}

// TestChallenger_LRU_HighConcurrency_RaceThrashing puts the cache under heavy
// concurrent load across 80 goroutines calling mixed operations under -race.
func TestChallenger_LRU_HighConcurrency_RaceThrashing(t *testing.T) {
	cache := NewChunkCache(5, 200*time.Millisecond)
	defer cache.Close()

	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	workers := 80

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID * 1000)))

			for {
				select {
				case <-stopCh:
					return
				default:
					key := fmt.Sprintf("key_%d", rng.Intn(15))
					op := rng.Intn(5)

					switch op {
					case 0:
						cache.Put(key, []byte(fmt.Sprintf("payload_%d", rng.Intn(1000))))
					case 1:
						_, _ = cache.Get(key)
					case 2:
						_, _ = cache.GetOrFetch(context.Background(), key, func() ([]byte, error) {
							return []byte("fetched"), nil
						})
					case 3:
						_ = cache.Len()
						_ = cache.ByteSize()
					case 4:
						if rng.Intn(20) == 0 {
							cache.Clear()
						}
					}
				}
			}
		}(i)
	}

	time.Sleep(800 * time.Millisecond)
	close(stopCh)
	wg.Wait()
}

// TestChallenger_LRU_CacheClose verifies behavior after cache shutdown.
func TestChallenger_LRU_CacheClose(t *testing.T) {
	cache := NewChunkCache(3, time.Minute)
	cache.Put("c1", []byte("val1"))

	if err := cache.Close(); err != nil {
		t.Fatalf("failed to close cache: %v", err)
	}

	if cache.Len() != 0 {
		t.Errorf("expected 0 length after close, got %d", cache.Len())
	}
	if _, ok := cache.Get("c1"); ok {
		t.Errorf("expected Get to return false after close")
	}

	// GetOrFetch must return ErrCacheClosed
	_, err := cache.GetOrFetch(context.Background(), "c2", func() ([]byte, error) {
		return []byte("val2"), nil
	})
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}

	// Subsequent close must be idempotent
	if err := cache.Close(); err != nil {
		t.Errorf("idempotent Close failed: %v", err)
	}
}

// TestChallenger_WindowsExplorer_FullSimulation simulates the exact HTTP verb and header
// sequence generated by the Windows WebDAV Mini-Redirector (WebClient service).
func TestChallenger_WindowsExplorer_FullSimulation(t *testing.T) {
	passphrase := "windows-client-secret-passphrase-32b"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
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

	file1Content := bytes.Repeat([]byte("WindowsExplorerTestData1234567890"), 500) // 16.5 KB
	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"Documents/Report.docx": file1Content,
		"Media/Video.mp4":       []byte("Simulated Video Content for Explorer Range Requests"),
	}, uploaded, srv.ChunkCache())

	// 1. Initial WebClient connection probe: OPTIONS /
	req := httptest.NewRequest("OPTIONS", "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp := w.Result()
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("OPTIONS / failed: status=%d", resp.StatusCode)
	}
	if resp.Header.Get("DAV") != "1, 2" {
		t.Errorf("expected 'DAV: 1, 2', got: %q", resp.Header.Get("DAV"))
	}
	if resp.Header.Get("MS-Author-Via") != "DAV" {
		t.Errorf("expected 'MS-Author-Via: DAV', got: %q", resp.Header.Get("MS-Author-Via"))
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Errorf("expected 'Accept-Ranges: bytes', got: %q", resp.Header.Get("Accept-Ranges"))
	}

	// 2. Volume Root PROPFIND Depth: 0 (checks collection properties without children)
	propXML := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/></D:prop></D:propfind>`
	req = httptest.NewRequest("PROPFIND", "/", bytes.NewBufferString(propXML))
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp = w.Result()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND / Depth 0 failed: status=%d", resp.StatusCode)
	}
	// Depth 0 must NOT list child items like "latest" or "snapshots"
	bodyStr := string(body)
	if strings.Contains(bodyStr, "<D:href>/latest</D:href>") {
		t.Errorf("Depth 0 response must not contain children: %s", bodyStr)
	}

	// 3. Root enumeration PROPFIND Depth: 1 (enumerates root children)
	req = httptest.NewRequest("PROPFIND", "/", bytes.NewBufferString(propXML))
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp = w.Result()
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND / Depth 1 failed: status=%d", resp.StatusCode)
	}
	bodyStr = string(body)
	if !strings.Contains(bodyStr, "latest") || !strings.Contains(bodyStr, "snapshots") {
		t.Errorf("Depth 1 response must contain latest and snapshots: %s", bodyStr)
	}

	// 4. Subdirectory PROPFIND Depth: 1 on /latest
	req = httptest.NewRequest("PROPFIND", "/latest", bytes.NewBufferString(propXML))
	req.Header.Set("Depth", "1")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp = w.Result()
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /latest Depth 1 failed: status=%d", resp.StatusCode)
	}

	// 5. Windows Explorer metadata probes: desktop.ini, Thumbs.db, autorun.inf
	// Must return 404 fast without hang
	windowsProbes := []string{
		"/desktop.ini",
		"/latest/desktop.ini",
		"/latest/Documents/desktop.ini",
		"/Thumbs.db",
		"/latest/Thumbs.db",
		"/autorun.inf",
	}

	for _, probe := range windowsProbes {
		req = httptest.NewRequest("GET", probe, nil)
		w = httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		resp = w.Result()
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("Windows probe %s: expected 404 Not Found, got %d", probe, resp.StatusCode)
		}

		// Also test PROPFIND on probes
		req = httptest.NewRequest("PROPFIND", probe, nil)
		w = httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		resp = w.Result()
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("Windows PROPFIND probe %s: expected 404 Not Found, got %d", probe, resp.StatusCode)
		}
	}

	// 6. Range GET reads (Windows Mini-Redirector chunks reads in 1KB/4KB blocks)
	req = httptest.NewRequest("GET", "/latest/Documents/Report.docx", nil)
	req.Header.Set("Range", "bytes=0-1023")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp = w.Result()
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range GET failed: status=%d", resp.StatusCode)
	}
	if len(body) != 1024 {
		t.Fatalf("expected 1024 bytes, got %d", len(body))
	}
	if !bytes.Equal(body, file1Content[0:1024]) {
		t.Errorf("range data mismatch for bytes 0-1023")
	}

	// 7. Windows write/mutation attempts on read-only mount
	windowsMutations := []struct {
		method string
		path   string
	}{
		{"PUT", "/latest/New%20Text%20Document.txt"},
		{"LOCK", "/latest/Documents/Report.docx"},
		{"DELETE", "/latest/Documents/Report.docx"},
		{"MOVE", "/latest/Documents/Report.docx"},
		{"MKCOL", "/latest/New%20Folder"},
		{"PROPPATCH", "/latest/Documents/Report.docx"},
	}

	for _, m := range windowsMutations {
		req = httptest.NewRequest(m.method, m.path, bytes.NewBufferString("sample content"))
		w = httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		resp = w.Result()
		resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Windows mutation %s %s: expected 403 Forbidden, got %d", m.method, m.path, resp.StatusCode)
		}
	}
}

// TestChallenger_MacOSFinder_FullSimulation simulates macOS Finder mount_webdav behavior,
// AppleDouble metadata probes, .DS_Store writes, and QuickLook random seeking.
func TestChallenger_MacOSFinder_FullSimulation(t *testing.T) {
	passphrase := "macos-client-secret-passphrase-32b"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
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

	videoContent := bytes.Repeat([]byte("QuickLookMovieStreamingTestData"), 200) // 6.2 KB
	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"Movies/Clip.mov": videoContent,
	}, uploaded, srv.ChunkCache())

	// 1. Initial mount handshake
	req := httptest.NewRequest("OPTIONS", "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp := w.Result()
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("OPTIONS / failed: %d", resp.StatusCode)
	}

	// 2. Finder AppleDouble and metadata probes
	finderProbes := []string{
		"/._.DS_Store",
		"/.DS_Store",
		"/.VolumeIcon.icns",
		"/latest/._Clip.mov",
		"/latest/Movies/._Clip.mov",
		"/latest/.DS_Store",
	}

	for _, probe := range finderProbes {
		req = httptest.NewRequest("GET", probe, nil)
		w = httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		resp = w.Result()
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
			t.Errorf("Finder probe %s: expected 404/403, got %d", probe, resp.StatusCode)
		}
	}

	// 3. Finder write attempts: .DS_Store creation
	finderWrites := []struct {
		method string
		path   string
	}{
		{"PUT", "/.DS_Store"},
		{"PUT", "/latest/.DS_Store"},
		{"PUT", "/latest/Movies/.DS_Store"},
		{"MKCOL", "/latest/Untitled%20Folder"},
		{"PROPPATCH", "/latest/Movies/Clip.mov"},
	}

	for _, fw := range finderWrites {
		req = httptest.NewRequest(fw.method, fw.path, bytes.NewBuffer([]byte{0x00, 0x01}))
		w = httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		resp = w.Result()
		resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Finder write %s %s: expected 403 Forbidden, got %d", fw.method, fw.path, resp.StatusCode)
		}
	}

	// 4. QuickLook random seeking: test non-contiguous seeks
	req = httptest.NewRequest("GET", "/latest/Movies/Clip.mov", nil)
	req.Header.Set("Range", "bytes=100-299")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp = w.Result()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("QuickLook seek 1 failed: status=%d", resp.StatusCode)
	}
	if len(body) != 200 || !bytes.Equal(body, videoContent[100:300]) {
		t.Errorf("QuickLook seek 1 content mismatch")
	}

	// Jump backwards: 0-49
	req = httptest.NewRequest("GET", "/latest/Movies/Clip.mov", nil)
	req.Header.Set("Range", "bytes=0-49")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp = w.Result()
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("QuickLook seek 2 failed: status=%d", resp.StatusCode)
	}
	if len(body) != 50 || !bytes.Equal(body, videoContent[0:50]) {
		t.Errorf("QuickLook seek 2 content mismatch")
	}
}

// TestChallenger_SynologyFileStation_FullSimulation simulates Synology DSM File Station
// mounting Telegram-Drive as a remote WebDAV folder.
func TestChallenger_SynologyFileStation_FullSimulation(t *testing.T) {
	passphrase := "synology-dsm-secret-passphrase-32b"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
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

	isoContent := bytes.Repeat([]byte("SynologyDSMFileStationRemoteBackupPackagePayload"), 1000) // 48 KB
	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"Backups/system.iso": isoContent,
	}, uploaded, srv.ChunkCache())

	// 1. DSM connection verification uses HEAD /
	req := httptest.NewRequest("HEAD", "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp := w.Result()
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DSM HEAD / verification probe failed: status=%d", resp.StatusCode)
	}

	// Also HEAD /latest
	req = httptest.NewRequest("HEAD", "/latest", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp = w.Result()
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DSM HEAD /latest probe failed: status=%d", resp.StatusCode)
	}

	// 2. DSM folder listing via PROPFIND Depth: 1
	propXML := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`
	req = httptest.NewRequest("PROPFIND", "/latest", bytes.NewBufferString(propXML))
	req.Header.Set("Depth", "1")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp = w.Result()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("DSM PROPFIND /latest failed: status=%d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Backups") {
		t.Errorf("DSM directory listing should include Backups: %s", string(body))
	}

	// 3. DSM sequential streaming downloads via Range chunks
	chunkSize := 16 * 1024
	var downloaded bytes.Buffer

	for offset := 0; offset < len(isoContent); offset += chunkSize {
		end := offset + chunkSize - 1
		if end >= len(isoContent) {
			end = len(isoContent) - 1
		}

		req = httptest.NewRequest("GET", "/latest/Backups/system.iso", nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
		w = httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		resp = w.Result()
		part, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("DSM chunk download offset %d failed: status=%d", offset, resp.StatusCode)
		}
		downloaded.Write(part)
	}

	if !bytes.Equal(downloaded.Bytes(), isoContent) {
		t.Errorf("DSM reassembled content mismatch: got %d bytes, expected %d bytes", downloaded.Len(), len(isoContent))
	}

	// 4. DSM write attempt rejected
	req = httptest.NewRequest("PUT", "/latest/syno_upload.tmp", bytes.NewBufferString("data"))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp = w.Result()
	resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("DSM write attempt: expected 403 Forbidden, got %d", resp.StatusCode)
	}
}

// TestChallenger_VirtualFS_SingleSnapshotMode verifies that specifying SnapshotID mounts
// that single snapshot directly at root (/) instead of the multi-snapshot /latest hierarchy.
func TestChallenger_VirtualFS_SingleSnapshotMode(t *testing.T) {
	passphrase := "single-snapshot-passphrase-32b"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
	defer mockTG.Close()

	cfg := ServerConfig{
		Addr:       "127.0.0.1",
		Port:       8089,
		ReadOnly:   true,
		SnapshotID: "snap-20261110-001", // Explicit single snapshot mode
	}

	srv, err := NewServer(cfg, engine, client, passphrase)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	expectedContent := []byte("Direct single snapshot file content")
	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"direct.txt": expectedContent,
	}, uploaded, srv.ChunkCache())

	// PROPFIND / with Depth 1 should directly list direct.txt at root!
	propXML := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`
	req := httptest.NewRequest("PROPFIND", "/", bytes.NewBufferString(propXML))
	req.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND / failed in single snapshot mode: status=%d", resp.StatusCode)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "direct.txt") {
		t.Errorf("expected direct.txt in root listing of single snapshot mode: %s", bodyStr)
	}

	// GET /direct.txt directly at root
	req = httptest.NewRequest("GET", "/direct.txt", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	resp = w.Result()
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /direct.txt failed: status=%d", resp.StatusCode)
	}
	if !bytes.Equal(body, expectedContent) {
		t.Errorf("content mismatch in single snapshot mode: got %q, expected %q", string(body), string(expectedContent))
	}
}

// TestChallenger_VirtualFS_BoundaryAndEdgeCases tortures seeking, 0-byte reads,
// non-existent snapshots, and permission errors on filesystem mutations.
func TestChallenger_VirtualFS_BoundaryAndEdgeCases(t *testing.T) {
	passphrase := "edge-cases-passphrase-32b"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
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

	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"small.txt": []byte("small file content"),
	}, uploaded, srv.ChunkCache())

	vfs := srv.VirtualFS()

	// 1. Filesystem mutations must return os.ErrPermission
	if err := vfs.Mkdir(context.Background(), "/newdir", 0755); err != os.ErrPermission {
		t.Errorf("expected os.ErrPermission for Mkdir, got: %v", err)
	}
	if err := vfs.RemoveAll(context.Background(), "/latest/small.txt"); err != os.ErrPermission {
		t.Errorf("expected os.ErrPermission for RemoveAll, got: %v", err)
	}
	if err := vfs.Rename(context.Background(), "/latest/small.txt", "/latest/renamed.txt"); err != os.ErrPermission {
		t.Errorf("expected os.ErrPermission for Rename, got: %v", err)
	}

	// 2. OpenFile with write flags must return os.ErrPermission
	writeFlags := []int{os.O_WRONLY, os.O_RDWR, os.O_CREATE, os.O_APPEND, os.O_TRUNC}
	for _, flag := range writeFlags {
		_, err := vfs.OpenFile(context.Background(), "/latest/small.txt", flag, 0644)
		if err != os.ErrPermission {
			t.Errorf("expected os.ErrPermission for flag %d, got: %v", flag, err)
		}
	}

	// 3. Stat on non-existent snapshot or file must return os.ErrNotExist
	_, err = vfs.Stat(context.Background(), "/snapshots/snap-99999999-missing")
	if err != os.ErrNotExist {
		t.Errorf("expected os.ErrNotExist for missing snapshot, got: %v", err)
	}
	_, err = vfs.Stat(context.Background(), "/latest/nonexistent_file.bin")
	if err != os.ErrNotExist {
		t.Errorf("expected os.ErrNotExist for missing file, got: %v", err)
	}

	// 4. Open regular file and test ReadSeeker edge cases
	f, err := vfs.OpenFile(context.Background(), "/latest/small.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("failed to open small.txt: %v", err)
	}
	defer f.Close()

	// Write to file returns os.ErrPermission
	_, err = f.Write([]byte("tamper"))
	if err != os.ErrPermission {
		t.Errorf("expected os.ErrPermission on file Write, got: %v", err)
	}

	// Negative seek offset returns error
	_, err = f.Seek(-10, io.SeekStart)
	if err == nil {
		t.Errorf("expected error for negative seek start")
	}

	// Invalid seek whence returns error
	_, err = f.Seek(0, 999)
	if err == nil {
		t.Errorf("expected error for invalid seek whence")
	}

	// Read with empty buffer returns 0, nil
	emptyBuf := make([]byte, 0)
	n, err := f.Read(emptyBuf)
	if n != 0 || err != nil {
		t.Errorf("read with 0-byte buffer: expected (0, nil), got (%d, %v)", n, err)
	}

	// Readdir on regular file returns error
	_, err = f.Readdir(1)
	if err == nil {
		t.Errorf("expected error for Readdir on regular file")
	}
}

// TestChallenger_Concurrent_Read_Stress executes concurrent reads and seeks from
// multiple goroutines on the same file instance to verify thread safety under -race.
func TestChallenger_Concurrent_Read_Stress(t *testing.T) {
	passphrase := "concurrent-read-passphrase-32b"
	content := bytes.Repeat([]byte("0123456789abcdefghijklmnopqrstuvwxyz"), 1000) // 36 KB

	cache := NewChunkCache(4, 5*time.Minute)
	defer cache.Close()
	cache.Put("conc_chunk_0", content)

	fe := snapshot.FileEntry{
		Path:    "concurrent.bin",
		Size:    int64(len(content)),
		ModTime: time.Now().Unix(),
		Chunks: []snapshot.ChunkRef{
			{Index: 0, Offset: 0, Size: int64(len(content)), TelegramFileID: "conc_chunk_0"},
		},
	}

	f := newWebdavRegularFile(context.Background(), "concurrent.bin", fe, nil, cache, passphrase)
	defer f.Close()

	var wg sync.WaitGroup
	workers := 40

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			buf := make([]byte, 64)
			for j := 0; j < 30; j++ {
				offset := int64((id*100 + j*50) % (len(content) - 100))
				_, seekErr := f.Seek(offset, io.SeekStart)
				if seekErr != nil {
					t.Errorf("concurrent seek error: %v", seekErr)
					return
				}
				_, readErr := f.Read(buf)
				if readErr != nil && readErr != io.EOF {
					t.Errorf("concurrent read error: %v", readErr)
					return
				}
			}
		}(i)
	}

	wg.Wait()
}

// TestChallenger_Three_Chunk_Boundary_Crossing verifies a single Read() call spanning
// across 3 separate chunk boundaries (Chunk 0 -> Chunk 1 -> Chunk 2).
func TestChallenger_Three_Chunk_Boundary_Crossing(t *testing.T) {
	passphrase := "three-chunk-passphrase-32b"
	chunk0 := bytes.Repeat([]byte("A"), 100)
	chunk1 := bytes.Repeat([]byte("B"), 100)
	chunk2 := bytes.Repeat([]byte("C"), 100)

	cache := NewChunkCache(4, time.Minute)
	defer cache.Close()
	cache.Put("c0", chunk0)
	cache.Put("c1", chunk1)
	cache.Put("c2", chunk2)

	fe := snapshot.FileEntry{
		Path:    "spanner.bin",
		Size:    300,
		ModTime: time.Now().Unix(),
		Chunks: []snapshot.ChunkRef{
			{Index: 0, Offset: 0, Size: 100, TelegramFileID: "c0"},
			{Index: 1, Offset: 100, Size: 100, TelegramFileID: "c1"},
			{Index: 2, Offset: 200, Size: 100, TelegramFileID: "c2"},
		},
	}

	f := newWebdavRegularFile(context.Background(), "spanner.bin", fe, nil, cache, passphrase)
	defer f.Close()

	// Seek to offset 80 (20 bytes before end of chunk 0)
	pos, err := f.Seek(80, io.SeekStart)
	if err != nil || pos != 80 {
		t.Fatalf("seek failed: %v", err)
	}

	// Read 140 bytes: 20 from chunk 0, 100 from chunk 1, 20 from chunk 2
	buf := make([]byte, 140)
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("cross-chunk read failed: %v", err)
	}
	if n != 140 {
		t.Fatalf("expected 140 bytes, got %d", n)
	}

	expected := append(bytes.Repeat([]byte("A"), 20), bytes.Repeat([]byte("B"), 100)...)
	expected = append(expected, bytes.Repeat([]byte("C"), 20)...)

	if !bytes.Equal(buf, expected) {
		t.Errorf("spanning buffer content mismatch")
	}
}

// TestChallenger_CorruptedChunk_DecryptionError verifies that corrupted chunk payloads
// return an explicit error and do not hang or crash.
func TestChallenger_CorruptedChunk_DecryptionError(t *testing.T) {
	passphrase := "corrupted-test-passphrase-32b"

	// Mock file entry with an uncached chunk pointing to non-existent client
	fe := snapshot.FileEntry{
		Path:    "broken.bin",
		Size:    100,
		ModTime: time.Now().Unix(),
		Chunks: []snapshot.ChunkRef{
			{Index: 0, Offset: 0, Size: 100, TelegramFileID: "missing_chunk"},
		},
	}

	// client is nil -> fetchChunk returns "telegram client is not configured"
	f := newWebdavRegularFile(context.Background(), "broken.bin", fe, nil, nil, passphrase)
	defer f.Close()

	buf := make([]byte, 50)
	n, err := f.Read(buf)
	if err == nil {
		t.Errorf("expected error for missing client/chunk, got n=%d", n)
	}
	if !strings.Contains(err.Error(), "telegram client is not configured") {
		t.Errorf("expected 'telegram client is not configured', got %v", err)
	}
}

// TestChallenger_0MB_Idle_RAM_AggressiveReclamation tests that allocating multi-megabyte
// chunks and calling Clear() immediately releases RAM and drops chunk count to 0.
func TestChallenger_0MB_Idle_RAM_AggressiveReclamation(t *testing.T) {
	cache := NewChunkCache(10, 50*time.Millisecond)
	defer cache.Close()

	// Record initial memory
	runtime.GC()
	var mInit runtime.MemStats
	runtime.ReadMemStats(&mInit)

	// Populate 10 MB in cache
	chunkSize := 2 * 1024 * 1024 // 2MB
	for i := 0; i < 5; i++ {
		payload := bytes.Repeat([]byte{byte(i + 1)}, chunkSize)
		cache.Put(fmt.Sprintf("large_chunk_%d", i), payload)
	}

	if cache.Len() != 5 {
		t.Fatalf("expected 5 items, got %d", cache.Len())
	}
	if cache.ByteSize() != 10*1024*1024 {
		t.Fatalf("expected 10MB byte size, got %d", cache.ByteSize())
	}

	// Explicitly clear and verify memory drops
	cache.Clear()

	if cache.Len() != 0 {
		t.Errorf("expected 0 items after Clear, got %d", cache.Len())
	}
	if cache.ByteSize() != 0 {
		t.Errorf("expected 0 ByteSize after Clear, got %d", cache.ByteSize())
	}

	runtime.GC()
	var mPost runtime.MemStats
	runtime.ReadMemStats(&mPost)

	t.Logf("Memory stats: Init=%d KB, PostClear=%d KB", mInit.HeapAlloc/1024, mPost.HeapAlloc/1024)
}
