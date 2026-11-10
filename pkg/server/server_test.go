package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/restore"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/snapshot"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/telegram"
)

func TestServerEndpoints(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "tgdrive_srv_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := config.DefaultConfig()
	cfg.SourcePath = tmpDir
	cfg.BotTokens = []string{"123456:mock_token_for_tests"}
	cfg.ChannelID = "-100123456789"
	cfg.EncryptionKey = "test-secret-key-123"

	tgClient, _ := telegram.NewClient(cfg.BotTokens, cfg.ChannelID, nil)
	engine, err := snapshot.NewEngine(cfg, tgClient)
	if err != nil {
		t.Fatal(err)
	}

	restorer := restore.NewRestorer(cfg, engine, tgClient)
	srv := NewServer(cfg, engine, restorer, tgClient, nil)

	// Find free port
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	go func() {
		_ = srv.Start(port)
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	}()

	time.Sleep(150 * time.Millisecond)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	// 1. Test GET / (HTML)
	resp, err := http.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("Failed to GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK from /, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "TeleClone Pro") {
		t.Errorf("Expected 'TeleClone Pro' in HTML, got: %s", string(body[:min(len(body), 200)]))
	}

	// 2. Test GET /api/status (JSON)
	statusResp, err := http.Get(baseURL + "/api/status")
	if err != nil {
		t.Fatalf("Failed to GET /api/status: %v", err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK from /api/status, got %d", statusResp.StatusCode)
	}

	// 3. Test GET /api/snapshots (JSON array)
	snapResp, err := http.Get(baseURL + "/api/snapshots")
	if err != nil {
		t.Fatalf("Failed to GET /api/snapshots: %v", err)
	}
	defer snapResp.Body.Close()
	if snapResp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK from /api/snapshots, got %d", snapResp.StatusCode)
	}

	// 4. Test GET /api/config (Sanitized JSON)
	cfgResp, err := http.Get(baseURL + "/api/config")
	if err != nil {
		t.Fatalf("Failed to GET /api/config: %v", err)
	}
	defer cfgResp.Body.Close()
	cfgBody, _ := io.ReadAll(cfgResp.Body)
	if strings.Contains(string(cfgBody), "test-secret-key-123") {
		t.Error("Security leak: encryption key not sanitized in /api/config!")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
