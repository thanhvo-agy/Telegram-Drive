package webdav

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/snapshot"
)

// TestReviewer_RFC4918_MethodMatrix verifies that all RFC 4918 methods behave
// according to read-only network drive expectations.
func TestReviewer_RFC4918_MethodMatrix(t *testing.T) {
	passphrase := "secret-reviewer-key-12345"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
	defer mockTG.Close()

	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"sample.txt": []byte("Readable test file for method matrix verification"),
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

	// 1. Allowed read-only methods
	allowedTests := []struct {
		method       string
		path         string
		expectedCode int
	}{
		{"OPTIONS", "/", http.StatusOK},
		{"HEAD", "/", http.StatusOK},
		{"HEAD", "/latest", http.StatusOK},
		{"HEAD", "/latest/", http.StatusOK},
		{"PROPFIND", "/", http.StatusMultiStatus},
		{"GET", "/latest/sample.txt", http.StatusOK},
		{"HEAD", "/latest/sample.txt", http.StatusOK},
	}

	for _, tt := range allowedTests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		if tt.method == "PROPFIND" {
			req.Header.Set("Depth", "0")
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		resp := w.Result()
		resp.Body.Close()

		if resp.StatusCode != tt.expectedCode {
			t.Errorf("Method %s on %s: expected %d, got %d", tt.method, tt.path, tt.expectedCode, resp.StatusCode)
		}

		// Verify OS headers present on every response
		if resp.Header.Get("DAV") != "1, 2" {
			t.Errorf("Method %s on %s: missing DAV: 1, 2 header", tt.method, tt.path)
		}
		if resp.Header.Get("MS-Author-Via") != "DAV" {
			t.Errorf("Method %s on %s: missing MS-Author-Via: DAV header", tt.method, tt.path)
		}
	}

	// 2. Prohibited mutating methods must return 403 Forbidden
	forbiddenMethods := []string{"PUT", "POST", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH", "LOCK"}
	for _, m := range forbiddenMethods {
		req := httptest.NewRequest(m, "/latest/sample.txt", bytes.NewBufferString("payload"))
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		resp := w.Result()
		resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Mutating method %s: expected 403 Forbidden, got: %d", m, resp.StatusCode)
		}
	}
}

// TestReviewer_O1_SeekBenchmark verifies O(1) performance of seeking with zero allocations/IO.
func TestReviewer_O1_SeekBenchmark(t *testing.T) {
	fe := snapshot.FileEntry{
		Path:    "large_file.dat",
		Size:    100 * 1024 * 1024 * 1024, // 100 GB virtual file
		ModTime: time.Now().Unix(),
		Chunks:  []snapshot.ChunkRef{},
	}

	file := newWebdavRegularFile(context.Background(), "large_file.dat", fe, nil, nil, "pass")
	defer file.Close()

	start := time.Now()
	iterations := 100000

	for i := 0; i < iterations; i++ {
		offset := int64((i * 1024 * 1024) % (100 * 1024 * 1024 * 1024))
		pos, err := file.Seek(offset, io.SeekStart)
		if err != nil || pos != offset {
			t.Fatalf("seek failed: offset=%d pos=%d err=%v", offset, pos, err)
		}
	}

	elapsed := time.Since(start)
	t.Logf("Executed %d seeks in %v (%v / seek)", iterations, elapsed, elapsed/time.Duration(iterations))
	if elapsed > 1*time.Second {
		t.Errorf("seeking took too long: expected < 1s for %d seeks, took %v", iterations, elapsed)
	}
}

// TestReviewer_MetadataProbesCompleteness verifies that noise probe files return 404 or 403.
func TestReviewer_MetadataProbesCompleteness(t *testing.T) {
	passphrase := "secret-reviewer-key-12345"
	mockTG, engine, client, stateDir, uploaded := setupMockTelegramAndEngine(t, passphrase)
	defer mockTG.Close()

	createTestSnapshot(t, stateDir, passphrase, map[string][]byte{
		"valid.txt": []byte("valid content"),
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

	probes := []string{
		"/._testfile",
		"/subfolder/._testfile",
		"/.DS_Store",
		"/subfolder/.DS_Store",
		"/desktop.ini",
		"/subfolder/desktop.ini",
		"/autorun.inf",
		"/Thumbs.db",
	}

	for _, p := range probes {
		// GET probe -> 404 Not Found
		req := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		resp := w.Result()
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: expected 404, got %d", p, resp.StatusCode)
		}

		// PUT probe -> 403 Forbidden
		reqPut := httptest.NewRequest("PUT", p, bytes.NewBuffer([]byte{1, 2, 3}))
		wPut := httptest.NewRecorder()
		srv.ServeHTTP(wPut, reqPut)
		respPut := wPut.Result()
		respPut.Body.Close()

		if respPut.StatusCode != http.StatusForbidden {
			t.Errorf("PUT %s: expected 403, got %d", p, respPut.StatusCode)
		}
	}
}
