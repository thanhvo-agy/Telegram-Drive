package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/webdav"
)

func handleMount(configPath string, args []string) {
	fs := flag.NewFlagSet("mount", flag.ContinueOnError)
	portFlag := fs.Int("port", 0, "Port for WebDAV server (default: 8089)")
	addrFlag := fs.String("addr", "", "Bind address (default: 0.0.0.0)")
	readOnlyFlag := fs.Bool("read-only", true, "Mount as read-only filesystem")
	cacheSizeFlag := fs.Int64("cache-size", 0, "Max in-memory chunk cache count (default: 4)")
	authFlag := fs.String("auth", "", "Optional HTTP Basic Auth username:password")
	cfgFlag := fs.String("config", "", "Path to configuration file")

	if err := fs.Parse(args); err == flag.ErrHelp {
		return
	}

	if *cfgFlag != "" {
		configPath = *cfgFlag
	}

	cfg, engine, _, tgClient, _ := setupEngine(configPath)

	// Determine port
	port := 8089
	if *portFlag > 0 {
		port = *portFlag
	} else if cfg.WebDAV.Port > 0 {
		port = cfg.WebDAV.Port
	}

	// Determine addr
	addr := "0.0.0.0"
	if *addrFlag != "" {
		addr = *addrFlag
	} else if cfg.WebDAV.Addr != "" {
		addr = cfg.WebDAV.Addr
	}

	// Determine cache size
	var cacheSize int64 = 4
	if *cacheSizeFlag > 0 {
		cacheSize = *cacheSizeFlag
	} else if cfg.WebDAV.CacheSizeMB > 0 {
		cacheSize = int64(cfg.WebDAV.CacheSizeMB / 49)
		if cacheSize <= 0 {
			cacheSize = 4
		}
	}

	// Optional authentication
	var username, password string
	if *authFlag != "" {
		parts := strings.SplitN(*authFlag, ":", 2)
		username = parts[0]
		if len(parts) > 1 {
			password = parts[1]
		}
	} else if cfg.WebDAV.Username != "" && cfg.WebDAV.Password != "" {
		username = cfg.WebDAV.Username
		password = cfg.WebDAV.Password
	}

	// Optional snapshot ID from positional argument
	var snapshotID string
	remainingArgs := fs.Args()
	if len(remainingArgs) > 0 && !strings.HasPrefix(remainingArgs[0], "-") {
		snapshotID = remainingArgs[0]
	}

	mountMode := "[MULTI-SNAPSHOT VIRTUAL ROOT] (/latest, /snapshots/<id>)"
	if snapshotID != "" {
		mountMode = fmt.Sprintf("[SINGLE SNAPSHOT ROOT] (%s)", snapshotID)
	}

	serverCfg := webdav.ServerConfig{
		Addr:        addr,
		Port:        port,
		ReadOnly:    *readOnlyFlag,
		CacheSize:   cacheSize,
		Username:    username,
		Password:    password,
		SnapshotID:  snapshotID,
		IdleTimeout: 60 * time.Second,
	}

	srv, err := webdav.NewServer(serverCfg, engine, tgClient, cfg.EncryptionKey)
	if err != nil {
		fmt.Printf("[FATAL] Failed to initialize WebDAV server: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("=================================================================")
	fmt.Printf("  TeleClone Pro WebDAV RFC 4918 Server (%s)\n", Version)
	fmt.Printf("  Architecture: %s | Zero-CGO Static Go Microserver\n", BuildArch)
	fmt.Printf("  Mount Mode:   %s\n", mountMode)
	fmt.Printf("  URL:          http://%s:%d/\n", addr, port)
	fmt.Printf("  Read-Only:    %t (Time Machine snapshot immutability enforced)\n", *readOnlyFlag)
	fmt.Printf("  Chunk Cache:  %d chunks (approx %d MB max, 0 MB idle RAM)\n", cacheSize, cacheSize*49)
	fmt.Printf("  OS Clients:   Windows Explorer, macOS Finder, Synology DSM\n")
	if username != "" {
		fmt.Printf("  HTTP Auth:    Enabled (user: %s)\n", username)
	} else {
		fmt.Printf("  HTTP Auth:    Disabled (open access)\n")
	}
	fmt.Println("=================================================================")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[INFO] Interrupt received, shutting down WebDAV server...")
		cancel()
		_ = srv.Stop()
	}()

	if err := srv.Start(ctx); err != nil && err != context.Canceled {
		fmt.Printf("[FATAL] WebDAV server error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("[INFO] WebDAV server stopped cleanly.")
}
