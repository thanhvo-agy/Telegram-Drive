package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/qos"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/restore"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/snapshot"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/server"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/telegram"
)

var (
	Version   = "2.0.0-pro"
	BuildArch = fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
)

func printUsage() {
	fmt.Printf(`TeleClone (tgdrive) - High-Performance Telegram Cloud Backup Engine (%s)
Architecture: %s | Zero-Knowledge AES-256-GCM | Rclone-Spirit Architecture

USAGE:
   tgdrive [command] [arguments...] [flags...]

COMMANDS:
   sync / backup       Run Time Machine CoW snapshot backup to Telegram
   restore             Restore a file, subfolder, or full snapshot
   snapshots           List all historical point-in-time snapshots
   ls                  List files and folders within a snapshot
   diff                Compare differences between two snapshots
   prune               Enforce Time Machine retention policy
   test-bot            Verify connectivity of Telegram Bot Mesh and Channel
   serve               Start embedded micro web dashboard and REST API
   config              Initialize or inspect configuration
   version             Display version information

GLOBAL FLAGS:
   --config <path>     Path to configuration file (default: tgdrive.json)
   --port <number>     Port for web dashboard (default: 8088)

EXAMPLES:
   tgdrive backup
   tgdrive restore snap-20261110-020000 photos/trip.jpg /volume1/restore
   tgdrive ls snap-20261110-020000 /
   tgdrive diff snap-20261109-020000 snap-20261110-020000
   tgdrive serve --port 8088
`, Version, BuildArch)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	configPath := flag.String("config", "", "Custom path to tgdrive.json")
	serverPort := flag.Int("port", 0, "Port for web server")

	// Parse flags for subcommands that accept them
	if cmd == "serve" || cmd == "backup" || cmd == "sync" || cmd == "config" {
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		configPath = fs.String("config", "", "Custom path to tgdrive.json")
		serverPort = fs.Int("port", 0, "Port for web server")
		_ = fs.Parse(args)
		args = fs.Args()
	}

	switch cmd {
	case "version", "-v", "--version":
		fmt.Printf("tgdrive version %s (%s) Go %s\n", Version, BuildArch, runtime.Version())
		return

	case "help", "-h", "--help":
		printUsage()
		return

	case "config":
		handleConfig(*configPath)
		return

	case "backup", "sync":
		handleBackup(*configPath)
		return

	case "snapshots", "list":
		handleListSnapshots(*configPath)
		return

	case "ls":
		handleListFiles(*configPath, args)
		return

	case "diff":
		handleDiff(*configPath, args)
		return

	case "restore":
		handleRestore(*configPath, args)
		return

	case "prune":
		handlePrune(*configPath)
		return

	case "test-bot":
		handleTestBot(*configPath)
		return

	case "serve":
		handleServe(*configPath, *serverPort)
		return

	default:
		fmt.Printf("Unknown command %q\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func setupEngine(configPath string) (*config.Config, *snapshot.Engine, *restore.Restorer, *telegram.Client, *qos.BandwidthThrottler) {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Printf("[FATAL] Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Apply nice priority on Linux
	if cfg.NicePriority != 0 {
		_ = qos.SetProcessNicePriority(cfg.NicePriority)
	}

	throttler, err := qos.ParseBandwidthSchedule(cfg.BandwidthSchedule)
	if err != nil {
		fmt.Printf("[WARN] Failed to parse bandwidth schedule: %v\n", err)
	}

	tgClient, err := telegram.NewClient(cfg.BotTokens, cfg.ChannelID, throttler)
	if err != nil {
		// Non-fatal warning so users can run 'serve' or inspect snapshots before configuring bots
		// fmt.Printf("[WARN] Telegram client offline: %v\n", err)
		tgClient = nil
	}

	engine, err := snapshot.NewEngine(cfg, tgClient)
	if err != nil {
		fmt.Printf("[FATAL] Failed to initialize Snapshot engine: %v\n", err)
		os.Exit(1)
	}

	restorer := restore.NewRestorer(cfg, engine, tgClient)
	return cfg, engine, restorer, tgClient, throttler
}

func handleConfig(configPath string) {
	target := config.FindConfigFile(configPath)
	if _, err := os.Stat(target); err == nil {
		fmt.Printf("[INFO] Configuration file exists at: %s\n", target)
		cfg, _ := config.Load(target)
		fmt.Printf("Node Name:   %s (%s)\n", cfg.NodeName, cfg.NodeMode)
		fmt.Printf("Source Path: %s\n", cfg.SourcePath)
		fmt.Printf("Bots Count:  %d token(s) configured\n", len(cfg.BotTokens))
		fmt.Printf("Channel ID:  %s\n", cfg.ChannelID)
		fmt.Printf("Schedule:    %s\n", cfg.BandwidthSchedule)
		return
	}

	cfg := config.DefaultConfig()
	if err := cfg.Save(target); err != nil {
		fmt.Printf("[FATAL] Failed to create config template: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[SUCCESS] Generated template configuration at: %s\n", target)
	fmt.Println("Please edit the file with your Telegram bot tokens, channel ID, and encryption key.")
}

func handleBackup(configPath string) {
	cfg, engine, _, tgClient, _ := setupEngine(configPath)
	if err := cfg.Validate(); err != nil {
		fmt.Printf("[FATAL] Invalid configuration: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("=== [TeleClone Snapshot Backup Initiated] ===\n")
	fmt.Printf("Node:        %s (%s)\n", cfg.NodeName, cfg.NodeMode)
	fmt.Printf("Source:      %s\n", cfg.SourcePath)
	fmt.Printf("Bot Mesh:    %d active bot(s)\n", len(cfg.BotTokens))
	fmt.Printf("Channel:     %s\n", cfg.ChannelID)
	fmt.Printf("QoS Rate:    %s (Nice=%d)\n", cfg.BandwidthSchedule, cfg.NicePriority)
	fmt.Println("-------------------------------------------------------------")

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[WARN] Interrupt received, gracefully terminating...")
		cancel()
	}()

	manifest, err := engine.CreateSnapshot(ctx, func(p snapshot.ProgressEvent) {
		if p.ProcessedFiles%10 == 0 || p.ProcessedFiles == p.TotalFiles {
			fmt.Printf("\r[%5.1f%%] Files: %d/%d | Delta: %.1fMB | CoW: %.1fMB | %s",
				p.PercentComplete,
				p.ProcessedFiles,
				p.TotalFiles,
				float64(p.UploadedBytes)/(1024*1024),
				float64(p.DeduplicatedBytes)/(1024*1024),
				truncateStr(p.CurrentFile, 30),
			)
		}
	})

	if err != nil {
		fmt.Printf("\n[FAILED] Backup failed: %v\n", err)
		_ = tgClient.SendAlert(ctx, fmt.Sprintf("[ALERT] *Backup Failed on %s*: %v", cfg.NodeName, err))
		os.Exit(1)
	}

	fmt.Printf("\n-------------------------------------------------------------\n")
	fmt.Printf("[SUCCESS] Snapshot Created: %s\n", manifest.SnapshotID)
	fmt.Printf("Total Files:      %d\n", manifest.TotalFiles)
	fmt.Printf("Total Size:       %.2f MB\n", float64(manifest.TotalBytes)/(1024*1024))
	fmt.Printf("Uploaded Delta:   %.2f MB\n", float64(manifest.UploadedBytes)/(1024*1024))
	fmt.Printf("Deduplicated:     %.2f MB (CoW savings: %.1f%%)\n",
		float64(manifest.DeduplicatedBytes)/(1024*1024),
		calcSavings(manifest.TotalBytes, manifest.UploadedBytes))
	fmt.Printf("Duration:         %.1fs\n", manifest.DurationSec)

	// Automatic retention prune
	plan, err := engine.PruneSnapshots()
	if err == nil && len(plan.Delete) > 0 {
		fmt.Printf("[RETENTION] Pruned %d expired snapshot(s).\n", len(plan.Delete))
	}
}

func handleListSnapshots(configPath string) {
	_, engine, _, _, _ := setupEngine(configPath)
	list, err := engine.ListSnapshots()
	if err != nil {
		fmt.Printf("[FATAL] Failed to list snapshots: %v\n", err)
		os.Exit(1)
	}

	if len(list) == 0 {
		fmt.Println("No snapshots found. Run 'tgdrive backup' to create your first snapshot.")
		return
	}

	fmt.Printf("%-24s %-20s %-8s %-12s %-12s %-10s\n", "SNAPSHOT ID", "CREATED AT", "FILES", "TOTAL SIZE", "DELTA", "NODE")
	fmt.Println(strings.Repeat("-", 90))
	for _, s := range list {
		fmt.Printf("%-24s %-20s %-8d %-12s %-12s %-10s\n",
			s.SnapshotID,
			s.CreatedAt.Format("2006-01-02 15:04:05"),
			s.TotalFiles,
			formatBytes(s.TotalBytes),
			formatBytes(s.UploadedBytes),
			s.NodeName,
		)
	}
}

func handleListFiles(configPath string, args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: tgdrive ls <snapshot_id> [subpath]")
		os.Exit(1)
	}

	snapID := args[0]
	subPath := ""
	if len(args) > 1 {
		subPath = args[1]
	}

	_, engine, _, _, _ := setupEngine(configPath)
	m, err := engine.GetSnapshot(snapID)
	if err != nil {
		fmt.Printf("[FATAL] Retrieve snapshot %s: %v\n", snapID, err)
		os.Exit(1)
	}

	items := m.ListDirectory(subPath)
	fmt.Printf("Directory listing for %s in snapshot %s (%d items):\n", subPath, snapID, len(items))
	fmt.Printf("%-6s %-12s %-20s %s\n", "TYPE", "SIZE", "MODIFIED", "NAME")
	fmt.Println(strings.Repeat("-", 70))
	for _, it := range items {
		tStr := "FILE"
		sStr := formatBytes(it.Size)
		if it.IsDir {
			tStr = "DIR"
			sStr = "-"
		}
		fmt.Printf("%-6s %-12s %-20s %s\n", tStr, sStr, it.ModTime.Format("2006-01-02 15:04"), it.Name)
	}
}

func handleDiff(configPath string, args []string) {
	if len(args) < 2 {
		fmt.Println("Usage: tgdrive diff <snapshot_a> <snapshot_b>")
		os.Exit(1)
	}

	_, engine, _, _, _ := setupEngine(configPath)
	mA, err := engine.GetSnapshot(args[0])
	if err != nil {
		fmt.Printf("[FATAL] Retrieve snapshot %s: %v\n", args[0], err)
		os.Exit(1)
	}
	mB, err := engine.GetSnapshot(args[1])
	if err != nil {
		fmt.Printf("[FATAL] Retrieve snapshot %s: %v\n", args[1], err)
		os.Exit(1)
	}

	diffs := snapshot.DiffManifests(mA, mB)
	fmt.Printf("Comparing %s -> %s (%d changes):\n", args[0], args[1], len(diffs))
	fmt.Printf("%-10s %-12s %s\n", "ACTION", "SIZE", "PATH")
	fmt.Println(strings.Repeat("-", 70))
	for _, d := range diffs {
		if d.Status == snapshot.DiffUnchanged {
			continue // Only show actual changes
		}
		sizeStr := formatBytes(d.NewSize)
		if d.Status == snapshot.DiffDeleted {
			sizeStr = formatBytes(d.OldSize)
		}
		fmt.Printf("%-10s %-12s %s\n", string(d.Status), sizeStr, d.Path)
	}
}

func handleRestore(configPath string, args []string) {
	if len(args) < 3 {
		fmt.Println("Usage: tgdrive restore <snapshot_id> <target_file_or_folder> <dest_dir>")
		fmt.Println("Example: tgdrive restore snap-20261110-020000 docs/financial.pdf /volume1/recovered")
		os.Exit(1)
	}

	snapID := args[0]
	relPath := args[1]
	destDir := args[2]

	_, engine, restorer, _, _ := setupEngine(configPath)
	ctx := context.Background()

	m, err := engine.GetSnapshot(snapID)
	if err != nil {
		fmt.Printf("[FATAL] Retrieve snapshot %s: %v\n", snapID, err)
		os.Exit(1)
	}

	fmt.Printf("=== [Restoring from Snapshot %s] ===\n", snapID)
	fmt.Printf("Target Path: %s\n", relPath)
	fmt.Printf("Destination: %s\n", destDir)

	if _, exists := m.Files[relPath]; exists {
		// Single file restore
		destFile := destDir
		if info, err := os.Stat(destDir); err == nil && info.IsDir() {
			destFile = destDir + "/" + relPath
		}
		if err := restorer.RestoreFile(ctx, snapID, relPath, destFile); err != nil {
			fmt.Printf("[FAILED] Single file restore failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("[SUCCESS] File restored: %s\n", destFile)
	} else {
		// Directory restore
		err := restorer.RestoreDirectory(ctx, snapID, relPath, destDir, func(p restore.RestoreProgress) {
			fmt.Printf("\r[%5.1f%%] Restored: %d/%d files | %s",
				p.Percent, p.RestoredFiles, p.TotalFiles, truncateStr(p.CurrentFile, 35))
		})
		if err != nil {
			fmt.Printf("\n[FAILED] Directory restore failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\n[SUCCESS] Directory restored cleanly to: %s\n", destDir)
	}
}

func handlePrune(configPath string) {
	_, engine, _, _, _ := setupEngine(configPath)
	plan, err := engine.PruneSnapshots()
	if err != nil {
		fmt.Printf("[FATAL] Prune failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("[INFO] Retention Evaluation:\n")
	fmt.Printf("  Keeping: %d snapshot(s)\n", len(plan.Keep))
	fmt.Printf("  Pruned:  %d snapshot(s)\n", len(plan.Delete))
	for _, del := range plan.Delete {
		fmt.Printf("   - Deleted: %s (%s)\n", del.SnapshotID, del.CreatedAt.Format("2006-01-02 15:04"))
	}
}

func handleTestBot(configPath string) {
	cfg, _, _, tgClient, _ := setupEngine(configPath)
	fmt.Printf("Testing Telegram Bot Mesh with %d token(s)...\n", len(cfg.BotTokens))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	testMsg := fmt.Sprintf("[TEST] *TeleClone Bot Mesh Test*\nNode: `%s`\nTime: `%s`\nStatus: Connectivity verified.",
		cfg.NodeName, time.Now().Format("2006-01-02 15:04:05"))

	if err := tgClient.SendAlert(ctx, testMsg); err != nil {
		fmt.Printf("[FAILED] Telegram alert error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("[SUCCESS] Sent test message to channel %s successfully!\n", cfg.ChannelID)
}

func handleServe(configPath string, port int) {
	cfg, engine, restorer, tgClient, throttler := setupEngine(configPath)

	srv := server.NewServer(cfg, engine, restorer, tgClient, throttler)
	if port <= 0 {
		port = cfg.ServerPort
	}
	if port <= 0 {
		port = 8088
	}

	fmt.Println("=================================================================")
	fmt.Printf("  TeleClone Pro Embedded Web Dashboard (%s)\n", Version)
	fmt.Printf("  Architecture: %s | Zero-Dependency Go Microserver\n", BuildArch)
	fmt.Printf("  Node Name:    %s (%s)\n", cfg.NodeName, cfg.NodeMode)
	fmt.Printf("  URL:          http://0.0.0.0:%d\n", port)
	fmt.Printf("  Memory Usage: < 5 MB RAM (0 MB background when idle)\n")
	fmt.Println("=================================================================")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[INFO] Shutting down web server...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
		os.Exit(0)
	}()

	if err := srv.Start(port); err != nil {
		fmt.Printf("[FATAL] Server error: %v\n", err)
		os.Exit(1)
	}
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "..." + s[len(s)-max+3:]
}

func calcSavings(total, uploaded int64) float64 {
	if total <= 0 {
		return 0
	}
	saved := total - uploaded
	if saved < 0 {
		return 0
	}
	return float64(saved) / float64(total) * 100.0
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
