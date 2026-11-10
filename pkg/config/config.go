package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RetentionConfig defines Time Machine snapshot prune policy
type RetentionConfig struct {
	KeepHourly  int `json:"keep_hourly"`
	KeepDaily   int `json:"keep_daily"`
	KeepWeekly  int `json:"keep_weekly"`
	KeepMonthly int `json:"keep_monthly"`
}

// Config represents the complete tgdrive configuration
type Config struct {
	// Telegram Bot Mesh
	BotTokens []string `json:"bot_tokens"`
	ChannelID string   `json:"channel_id"`

	// Security & Encryption
	EncryptionKey string `json:"encryption_key"` // Master passphrase for AES-256-GCM

	// Backup Storage & Chunking
	SourcePath string   `json:"source_path"`
	ChunkSize  int64    `json:"chunk_size"` // Bytes per chunk (default 49MB)
	Excludes   []string `json:"excludes"`   // Excluded file patterns (@eaDir, #recycle, etc.)

	// Quality of Service & Throttling
	BandwidthSchedule string `json:"bandwidth_schedule"` // e.g. "07:00,5M 23:00,18M"
	NicePriority      int    `json:"nice_priority"`      // Default 19

	// Retention Policy
	Retention RetentionConfig `json:"retention"`

	// Web Dashboard
	ServerPort int    `json:"server_port"` // Default 8088
	WebEnabled bool   `json:"web_enabled"`
	NodeName   string `json:"node_name"`   // Identifier (e.g. "Synology-DS920+", "Buffalo-LS410D")
	NodeMode   string `json:"node_mode"`   // "PUSH" or "PULL"

	// Internal
	ConfigFilePath string `json:"-"`
}

// DefaultConfig returns recommended configuration values
func DefaultConfig() *Config {
	return &Config{
		BotTokens:         []string{},
		ChannelID:         "",
		EncryptionKey:     "",
		SourcePath:        "/volume1/data",
		ChunkSize:         49 * 1024 * 1024, // 49MB (safe for Telegram 50MB Bot limit)
		BandwidthSchedule: "07:00,5M 23:00,18M",
		NicePriority:      19,
		Excludes: []string{
			"@eaDir/**",
			"@SynoResource/**",
			"@tmp/**",
			"#recycle/**",
			".SynologyWorkingDirectory/**",
			".DS_Store",
			"Thumbs.db",
			"*.tmp",
		},
		Retention: RetentionConfig{
			KeepHourly:  24,
			KeepDaily:   7,
			KeepWeekly:  4,
			KeepMonthly: 12,
		},
		ServerPort: 8088,
		WebEnabled: true,
		NodeName:   "NAS-Node",
		NodeMode:   "PUSH",
	}
}

// FindConfigFile locates the configuration file in standard search locations
func FindConfigFile(customPath string) string {
	if customPath != "" {
		return customPath
	}

	// 1. Current working directory
	if _, err := os.Stat("tgdrive.json"); err == nil {
		return "tgdrive.json"
	}

	// 2. User home directory ~/.config/tgdrive/tgdrive.json
	home, err := os.UserHomeDir()
	if err == nil {
		p := filepath.Join(home, ".config", "tgdrive", "tgdrive.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// 3. /etc/tgdrive/tgdrive.json
	if _, err := os.Stat("/etc/tgdrive/tgdrive.json"); err == nil {
		return "/etc/tgdrive/tgdrive.json"
	}

	// Default fallback
	if home != "" {
		return filepath.Join(home, ".config", "tgdrive", "tgdrive.json")
	}
	return "tgdrive.json"
}

// Load reads and parses the configuration file
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()
	configFile := FindConfigFile(path)
	cfg.ConfigFilePath = configFile

	data, err := os.ReadFile(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Apply environment variables over default if file does not exist
			applyEnvOverrides(cfg)
			return cfg, nil
		}
		return nil, fmt.Errorf("read config file: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config json: %w", err)
	}

	applyEnvOverrides(cfg)
	return cfg, nil
}

// Save writes current config to disk
func (c *Config) Save(path string) error {
	target := path
	if target == "" {
		target = c.ConfigFilePath
	}
	if target == "" {
		target = "tgdrive.json"
	}

	dir := filepath.Dir(target)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("create config dir: %w", err)
		}
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(target, data, 0600); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	c.ConfigFilePath = target
	return nil
}

// Validate checks essential requirements
func (c *Config) Validate() error {
	if len(c.BotTokens) == 0 {
		return errors.New("no Telegram bot token configured (bot_tokens is empty)")
	}
	for i, tok := range c.BotTokens {
		if strings.TrimSpace(tok) == "" {
			return fmt.Errorf("bot token at index %d is empty", i)
		}
	}
	if strings.TrimSpace(c.ChannelID) == "" {
		return errors.New("channel_id is required")
	}
	if strings.TrimSpace(c.EncryptionKey) == "" {
		return errors.New("encryption_key is required for client-side zero-knowledge encryption")
	}
	if c.ChunkSize <= 0 || c.ChunkSize > 50*1024*1024 {
		c.ChunkSize = 49 * 1024 * 1024
	}
	return nil
}

func applyEnvOverrides(cfg *Config) {
	if tok := os.Getenv("TELEGRAM_BOT_TOKEN"); tok != "" {
		cfg.BotTokens = []string{tok}
	}
	if tokens := os.Getenv("TELEGRAM_BOT_TOKENS"); tokens != "" {
		parts := strings.Split(tokens, ",")
		var cleaned []string
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				cleaned = append(cleaned, s)
			}
		}
		if len(cleaned) > 0 {
			cfg.BotTokens = cleaned
		}
	}
	if cid := os.Getenv("TELEGRAM_CHANNEL_ID"); cid != "" {
		cfg.ChannelID = cid
	}
	if key := os.Getenv("ENCRYPTION_KEY"); key != "" {
		cfg.EncryptionKey = key
	}
	if src := os.Getenv("NAS_SOURCE_PATH"); src != "" {
		cfg.SourcePath = src
	}
	if bw := os.Getenv("RCLONE_BWLIMIT"); bw != "" {
		cfg.BandwidthSchedule = bw
	}
	if name := os.Getenv("NODE_NAME"); name != "" {
		cfg.NodeName = name
	}
	if mode := os.Getenv("NODE_MODE"); mode != "" {
		cfg.NodeMode = mode
	}
}
