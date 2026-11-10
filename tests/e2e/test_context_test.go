package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/server"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/snapshot"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/telegram"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/webdav"
)

var (
	buildOnce   sync.Once
	builtBinary string
	buildErr    error
)

// ensureBinary builds the tgdrive CLI binary once per test run.
func ensureBinary() (string, error) {
	buildOnce.Do(func() {
		// Check if bin/tgdrive already exists and is executable
		existing := filepath.Join(getRepoRoot(), "bin", "tgdrive")
		if info, err := os.Stat(existing); err == nil && !info.IsDir() {
			builtBinary = existing
			return
		}

		tmpBin := filepath.Join(os.TempDir(), fmt.Sprintf("tgdrive-e2e-%d", time.Now().UnixNano()))
		cmd := exec.Command("go", "build", "-o", tmpBin, "./cmd/tgdrive")
		cmd.Dir = getRepoRoot()
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("failed to build tgdrive binary: %v, output: %s", err, string(out))
			return
		}
		builtBinary = tmpBin
	})
	return builtBinary, buildErr
}

func getRepoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "/home/antigravity/thanhvo-agy/Telegram-Drive"
	}
	// Walk up to find go.mod
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "/home/antigravity/thanhvo-agy/Telegram-Drive"
}

// E2EContext manages sandbox environments and mock services for E2E tests.
type E2EContext struct {
	T          *testing.T
	RootDir    string
	TempDir    string
	SourceDir  string
	RestoreDir string
	ConfigPath string
	BinaryPath string
	MockTG     *MockTelegramServer
	Cfg        *config.Config
}

// NewE2EContext creates a fully isolated sandbox for an E2E test.
func NewE2EContext(t *testing.T) *E2EContext {
	tmpDir, err := os.MkdirTemp("", "tgdrive_e2e_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	sourceDir := filepath.Join(tmpDir, "source")
	restoreDir := filepath.Join(tmpDir, "restore")
	_ = os.MkdirAll(sourceDir, 0755)
	_ = os.MkdirAll(restoreDir, 0755)

	mockTG := NewMockTelegramServer()

	binPath, err := ensureBinary()
	if err != nil {
		t.Fatalf("ensureBinary failed: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.SourcePath = sourceDir
	cfg.BotTokens = []string{"123456789:ABCdefGHI_mock_token_1", "987654321:XYZabcJKL_mock_token_2"}
	cfg.ChannelID = "-1001999888777"
	cfg.EncryptionKey = "e2e-master-encryption-passphrase-32b"
	cfg.ChunkSize = 49 * 1024 * 1024
	cfg.ServerPort = 8088

	configPath := filepath.Join(tmpDir, "tgdrive.json")
	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(configPath, data, 0644)

	ctx := &E2EContext{
		T:          t,
		RootDir:    getRepoRoot(),
		TempDir:    tmpDir,
		SourceDir:  sourceDir,
		RestoreDir: restoreDir,
		ConfigPath: configPath,
		BinaryPath: binPath,
		MockTG:     mockTG,
		Cfg:        cfg,
	}

	t.Cleanup(func() {
		ctx.Cleanup()
	})

	return ctx
}

// Cleanup tears down the test environment.
func (c *E2EContext) Cleanup() {
	if c.MockTG != nil {
		c.MockTG.Close()
	}
	if c.TempDir != "" {
		_ = os.RemoveAll(c.TempDir)
	}
}

// CreateCompressibleFile writes a highly repetitive text/log file.
func (c *E2EContext) CreateCompressibleFile(relPath string, sizeBytes int) []byte {
	fullPath := filepath.Join(c.SourceDir, relPath)
	_ = os.MkdirAll(filepath.Dir(fullPath), 0755)

	chunk := []byte("2026-11-10T12:00:00.000Z [INFO] worker_thread: processing queue item batch records [OK]\n")
	buf := make([]byte, sizeBytes)
	for i := 0; i < sizeBytes; i += len(chunk) {
		copy(buf[i:], chunk)
	}

	if err := os.WriteFile(fullPath, buf, 0644); err != nil {
		c.T.Fatalf("failed to write compressible file %s: %v", relPath, err)
	}
	return buf
}

// CreateIncompressibleFile writes pseudo-random cryptographic data (entropy ~8 bits/byte).
func (c *E2EContext) CreateIncompressibleFile(relPath string, sizeBytes int) []byte {
	fullPath := filepath.Join(c.SourceDir, relPath)
	_ = os.MkdirAll(filepath.Dir(fullPath), 0755)

	buf := make([]byte, sizeBytes)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		c.T.Fatalf("failed to generate random bytes for %s: %v", relPath, err)
	}

	if err := os.WriteFile(fullPath, buf, 0644); err != nil {
		c.T.Fatalf("failed to write incompressible file %s: %v", relPath, err)
	}
	return buf
}

// CreateEmptyFile writes a 0-byte file.
func (c *E2EContext) CreateEmptyFile(relPath string) {
	fullPath := filepath.Join(c.SourceDir, relPath)
	_ = os.MkdirAll(filepath.Dir(fullPath), 0755)
	if err := os.WriteFile(fullPath, []byte{}, 0644); err != nil {
		c.T.Fatalf("failed to create empty file %s: %v", relPath, err)
	}
}

// RunCLI executes the tgdrive CLI binary with provided arguments.
func (c *E2EContext) RunCLI(args ...string) (stdout string, stderr string, exitCode int, err error) {
	fullArgs := []string{"--config", c.ConfigPath}
	fullArgs = append(fullArgs, args...)

	var outBuf, errBuf bytes.Buffer
	cmd := exec.Command(c.BinaryPath, fullArgs...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	// Configure environment to point to mock telegram if applicable
	cmd.Env = append(os.Environ(),
		"TELEGRAM_API_URL="+c.MockTG.URL(),
		"HTTP_PROXY="+c.MockTG.URL(),
		"HTTPS_PROXY="+c.MockTG.URL(),
	)

	runErr := cmd.Run()
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	} else {
		exitCode = 0
	}

	return outBuf.String(), errBuf.String(), exitCode, runErr
}

// SupportsCommand checks if the binary recognizes the specified subcommand.
func (c *E2EContext) SupportsCommand(subcommand string) bool {
	var outBuf, errBuf bytes.Buffer
	cmd := exec.Command(c.BinaryPath, subcommand, "--help")
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	_ = cmd.Run()

	combined := outBuf.String() + " " + errBuf.String()
	// If the output mentions unknown command or command not found
	if strings.Contains(strings.ToLower(combined), "unknown command") ||
		strings.Contains(strings.ToLower(combined), "unrecognized") {
		return false
	}
	return true
}

// SupportsWebDAV checks whether the WebDAV mount subcommand is recognized.
func (c *E2EContext) SupportsWebDAV() bool {
	if !c.SupportsCommand("mount") {
		return false
	}
	startWebDAVServerOnce(c)
	return true
}

// SupportsBot checks whether the interactive bot command or controller is supported.
func (c *E2EContext) SupportsBot() bool {
	if !c.SupportsCommand("bot") && !c.SupportsCommand("serve") {
		return false
	}
	startBotControllerForContext(c)
	return true
}

var (
	botControllersMu     sync.Mutex
	activeBotControllers = make(map[*E2EContext]*telegram.BotController)
)

func startBotControllerForContext(c *E2EContext) {
	botControllersMu.Lock()
	defer botControllersMu.Unlock()

	if activeBotControllers[c] != nil {
		return
	}

	cfg := c.Cfg
	if len(cfg.TelegramAuthorizedUsers) == 0 {
		cfg.TelegramAuthorizedUsers = []int64{1001, 1002}
	}
	if len(cfg.BotControl.AuthorizedUserIDs) == 0 {
		cfg.BotControl.AuthorizedUserIDs = []int64{1001, 1002}
	}
	cfg.BotControl.PollTimeoutSec = 1

	tgClient, err := telegram.NewClient(cfg.BotTokens, cfg.ChannelID, nil)
	if err != nil {
		return
	}
	tgClient.SetBaseURL(c.MockTG.URL())

	engine, err := snapshot.NewEngine(cfg, tgClient)
	if err != nil {
		return
	}

	srv := server.NewServer(cfg, engine, nil, tgClient, nil)
	ctrl := telegram.NewBotController(cfg, tgClient, engine, srv)

	ctx, cancel := context.WithCancel(context.Background())
	if err := ctrl.Start(ctx); err != nil {
		cancel()
		return
	}

	activeBotControllers[c] = ctrl

	c.T.Cleanup(func() {
		ctrl.Stop()
		cancel()
		botControllersMu.Lock()
		delete(activeBotControllers, c)
		botControllersMu.Unlock()
	})
}

// SupportsWORM checks whether the reindex command or worm flags are recognized.
func (c *E2EContext) SupportsWORM() bool {
	return c.SupportsCommand("reindex")
}

var (
	webdavStartOnce sync.Once
	sharedWebdavSrv *webdav.Server
)

func startWebDAVServerOnce(c *E2EContext) {
	webdavStartOnce.Do(func() {
		wdStateDir, err := os.MkdirTemp("", "tgdrive_e2e_webdav_*")
		if err != nil {
			return
		}

		snapID := "snap-e2e-latest"
		chunkSize49MB := int64(49 * 1024 * 1024)
		largeTotalSize := int64(52 * 1024 * 1024)

		manifest := &snapshot.SnapshotManifest{
			SnapshotID:  snapID,
			CreatedAt:   time.Now().Add(-1 * time.Hour),
			NodeName:    "E2E-NAS",
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

		testContent := []byte("TeleClone Pro WebDAV test file content for streaming GET and HEAD.\n")
		manifest.Files["testfile.txt"] = snapshot.FileEntry{
			Path:    "testfile.txt",
			Size:    int64(len(testContent)),
			ModTime: time.Now().Unix(),
			Chunks: []snapshot.ChunkRef{
				{Index: 0, Offset: 0, Size: int64(len(testContent)), TelegramFileID: "tf_testfile"},
			},
		}

		videoContent := make([]byte, 16384)
		for i := range videoContent {
			videoContent[i] = byte(i % 256)
		}
		manifest.Files["video.mp4"] = snapshot.FileEntry{
			Path:    "video.mp4",
			Size:    int64(len(videoContent)),
			ModTime: time.Now().Unix(),
			Chunks: []snapshot.ChunkRef{
				{Index: 0, Offset: 0, Size: int64(len(videoContent)), TelegramFileID: "tf_video"},
			},
		}

		chunk0 := make([]byte, chunkSize49MB)
		chunk1 := make([]byte, largeTotalSize-chunkSize49MB)
		manifest.Files["large.bin"] = snapshot.FileEntry{
			Path:    "large.bin",
			Size:    largeTotalSize,
			ModTime: time.Now().Unix(),
			Chunks: []snapshot.ChunkRef{
				{Index: 0, Offset: 0, Size: chunkSize49MB, TelegramFileID: "tf_large_0"},
				{Index: 1, Offset: chunkSize49MB, Size: int64(len(chunk1)), TelegramFileID: "tf_large_1"},
			},
		}

		manifest.Files["test.txt"] = snapshot.FileEntry{
			Path:    "test.txt",
			Size:    100,
			ModTime: time.Now().Unix(),
			Chunks: []snapshot.ChunkRef{
				{Index: 0, Offset: 0, Size: 100, TelegramFileID: "tf_test"},
			},
		}

		mixedContent := []byte("Mixed TGC1 and TGC2 contents for pairwise testing.")
		manifest.Files["mixed_file.txt"] = snapshot.FileEntry{
			Path:    "mixed_file.txt",
			Size:    int64(len(mixedContent)),
			ModTime: time.Now().Unix(),
			Chunks: []snapshot.ChunkRef{
				{Index: 0, Offset: 0, Size: int64(len(mixedContent)), TelegramFileID: "tf_mixed"},
			},
		}

		docContent := []byte("%PDF-1.4 Mock document PDF for testing active mounts and probes.\n")
		manifest.Files["document.pdf"] = snapshot.FileEntry{
			Path:    "document.pdf",
			Size:    int64(len(docContent)),
			ModTime: time.Now().Unix(),
			Chunks: []snapshot.ChunkRef{
				{Index: 0, Offset: 0, Size: int64(len(docContent)), TelegramFileID: "tf_doc"},
			},
		}

		audioContent := make([]byte, 70000)
		manifest.Files["audio.flac"] = snapshot.FileEntry{
			Path:    "audio.flac",
			Size:    int64(len(audioContent)),
			ModTime: time.Now().Unix(),
			Chunks: []snapshot.ChunkRef{
				{Index: 0, Offset: 0, Size: int64(len(audioContent)), TelegramFileID: "tf_audio"},
			},
		}

		for _, fe := range manifest.Files {
			manifest.TotalBytes += fe.Size
		}
		manifest.TotalFiles = len(manifest.Files)
		manifest.WORM.ManifestHMAC = manifest.ComputeHMAC("e2e-master-encryption-passphrase-32b")

		data, _ := json.MarshalIndent(manifest, "", "  ")
		_ = os.WriteFile(filepath.Join(wdStateDir, snapID+".json"), data, 0644)

		cfg := config.DefaultConfig()
		cfg.StateDir = wdStateDir
		cfg.EncryptionKey = "e2e-master-encryption-passphrase-32b"

		engine, err := snapshot.NewEngine(cfg, nil)
		if err != nil {
			return
		}

		wdCfg := webdav.ServerConfig{
			Addr:        "127.0.0.1",
			Port:        8089,
			ReadOnly:    true,
			CacheSize:   10,
			IdleTimeout: 10 * time.Minute,
		}

		srv, err := webdav.NewServer(wdCfg, engine, nil, cfg.EncryptionKey)
		if err != nil {
			return
		}

		cache := srv.ChunkCache()
		cache.Put("tf_testfile", testContent)
		cache.Put("tf_video", videoContent)
		cache.Put("tf_large_0", chunk0)
		cache.Put("tf_large_1", chunk1)
		cache.Put("tf_test", make([]byte, 100))
		cache.Put("tf_mixed", mixedContent)
		cache.Put("tf_doc", docContent)
		cache.Put("tf_audio", audioContent)

		sharedWebdavSrv = srv
		go func() {
			_ = srv.Start(context.Background())
		}()

		for i := 0; i < 50; i++ {
			time.Sleep(10 * time.Millisecond)
			conn, err := net.DialTimeout("tcp", "127.0.0.1:8089", 50*time.Millisecond)
			if err == nil {
				conn.Close()
				break
			}
		}
	})
}
