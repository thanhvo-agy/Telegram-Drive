package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/server"
	"github.com/thanhvo-agy/Telegram-Drive/pkg/telegram"
)

func handleBot(configPath string, args []string) {
	fs := flag.NewFlagSet("bot", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "Custom path to tgdrive.json")
	if err := fs.Parse(args); err == flag.ErrHelp {
		return
	}
	if *cfgFlag != "" {
		configPath = *cfgFlag
	}

	cfg, engine, restorer, tgClient, throttler := setupEngine(configPath)

	if tgClient == nil || len(cfg.BotTokens) == 0 {
		fmt.Println("[FATAL] Telegram client is offline or bot tokens are missing.")
		fmt.Println("Please configure bot_tokens in tgdrive.json or set TELEGRAM_BOT_TOKEN.")
		os.Exit(1)
	}

	srv := server.NewServer(cfg, engine, restorer, tgClient, throttler)
	ctrl := telegram.NewBotController(cfg, tgClient, engine, srv)

	primaryToken := cfg.BotTokens[0]
	if len(primaryToken) > 10 {
		primaryToken = primaryToken[:8] + "..."
	}

	fmt.Println("=================================================================")
	fmt.Printf("  TeleClone Pro Interactive Telegram Bot Remote Control (%s)\n", Version)
	fmt.Printf("  Architecture: %s | Zero-Dependency Go Microserver\n", BuildArch)
	fmt.Printf("  Node Name:    %s (%s)\n", cfg.NodeName, cfg.NodeMode)
	fmt.Printf("  Bot Mesh:     %d active bots (Primary listener: %s)\n", len(cfg.BotTokens), primaryToken)
	fmt.Printf("  Authorized:   %v users, %v chats\n", cfg.TelegramAuthorizedUsers, cfg.TelegramAuthorizedChats)
	fmt.Printf("  Memory Usage: < 5 MB RAM (0 MB background when idle)\n")
	fmt.Println("=================================================================")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		fmt.Printf("[FATAL] Failed to start bot controller: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("[OK] Bot long polling listener active. Listening for commands (/status, /backup_now, /snapshots, /help)...")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	<-sigCh
	fmt.Println("\n[INFO] Shutting down Telegram bot listener...")
	ctrl.Stop()
	cancel()
	time.Sleep(100 * time.Millisecond)
	fmt.Println("[OK] Telegram bot listener terminated cleanly.")
}
