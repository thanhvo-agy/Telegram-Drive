package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

// handleReindex executes disaster recovery reindexing of manifests from Telegram
func handleReindex(configPath string, args []string) {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "Custom path to tgdrive.json")
	if err := fs.Parse(args); err == flag.ErrHelp {
		return
	}
	if *cfgFlag != "" {
		configPath = *cfgFlag
	}

	cfg, engine, _, tgClient, _ := setupEngine(configPath)
	if err := cfg.Validate(); err != nil {
		fmt.Printf("[FATAL] Invalid configuration: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Println("=== [Disaster Recovery Reindex Initiated] ===")
	fmt.Printf("Node:     %s (%s)\n", cfg.NodeName, cfg.NodeMode)
	fmt.Printf("Channel:  %s\n", cfg.ChannelID)
	fmt.Printf("StateDir: %s\n", engine.StateDir())

	if tgClient == nil {
		fmt.Println("[WARN] Telegram client offline: attempting direct recovery connection")
	}

	summary, err := engine.ReindexFromTelegram(ctx)
	if err != nil {
		fmt.Printf("[FATAL] Reindex encountered error: %v\n", err)
		if summary != nil {
			fmt.Printf("[INFO] Partial Reindex Summary:\n")
			fmt.Printf("  Messages Scanned:   %d\n", summary.ScannedMessages)
			fmt.Printf("  Manifests Found:    %d\n", summary.ManifestsFound)
			fmt.Printf("  Manifests Restored: %d\n", summary.ManifestsRestored)
			fmt.Printf("  Manifests Skipped:  %d\n", summary.ManifestsSkipped)
			fmt.Printf("  Files Restored:     %d\n", summary.TotalFilesRestored)
			fmt.Printf("  Bytes Restored:     %s\n", formatBytes(summary.TotalBytesRestored))
		}
		fmt.Println("[FAILED] Reindex failed.")
		os.Exit(1)
	}

	if summary != nil {
		fmt.Printf("[INFO] Reindex Summary:\n")
		fmt.Printf("  Messages Scanned:   %d\n", summary.ScannedMessages)
		fmt.Printf("  Manifests Found:    %d\n", summary.ManifestsFound)
		fmt.Printf("  Manifests Restored: %d\n", summary.ManifestsRestored)
		fmt.Printf("  Manifests Skipped:  %d\n", summary.ManifestsSkipped)
		fmt.Printf("  Files Restored:     %d\n", summary.TotalFilesRestored)
		fmt.Printf("  Bytes Restored:     %s\n", formatBytes(summary.TotalBytesRestored))
	}
	fmt.Println("[SUCCESS] Reindex completed.")
}
