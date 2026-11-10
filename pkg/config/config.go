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

// WORMConfig defines enterprise WORM snapshot retention locking settings
type WORMConfig struct {
	Enabled       bool   `json:"enabled"`
	Mode          string `json:"mode"`           // "compliance" or "governance"
	RetentionDays int    `json:"retention_days"` // e.g. 30, 90, 365
	LegalHold     bool   `json:"legal_hold"`
}

// WebDAVConfig defines embedded WebDAV RFC 4918 server settings
type WebDAVConfig struct {
	Enabled     bool   `json:"enabled"`
	Port        int    `json:"port"`
	Addr        string `json:"addr"`
	CacheSizeMB int    `json:"cache_size_mb"`
	Username    string `json:"username"`
	Password    string `json:"password"`
}

// BotControlConfig defines 2-way Telegram bot remote control settings
type BotControlConfig struct {
	Enabled           bool    `json:"enabled"`
	PollTimeoutSec    int     `json:"poll_timeout_sec"`
	AuthorizedUserIDs []int64 `json:"authorized_user_ids"`
	AuthorizedChatIDs []int64 `json:"authorized_chat_ids"`
}

// Config represents the complete tgdrive configuration
type Config struct {
	// Telegram Bot Mesh
	BotTokens []string `json:"bot_tokens"`
	ChannelID string   `json:"channel_id"`

	// Telegram 2-Way Bot Remote Control
	BotControl              BotControlConfig `json:"bot_control"`
	TelegramAuthorizedUsers []int64          `json:"telegram_authorized_users"`
	TelegramAuthorizedChats []int64          `json:"telegram_authorized_chats"`

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

	// Anti-Ransomware WORM Snapshot Locking
	WORM WORMConfig `json:"worm"`

	// Embedded WebDAV Server
	WebDAV WebDAVConfig `json:"webdav"`

	// Web Dashboard
	ServerPort int    `json:"server_port"` // Default 8088
	WebEnabled bool   `json:"web_enabled"`
	NodeName   string `json:"node_name"`   // Identifier (e.g. "Synology-DS920+", "Buffalo-LS410D")
	NodeMode   string `json:"node_mode"`   // "PUSH" or "PULL"

	// Internal
	ConfigFilePath string `json:"-"`
	StateDir       string `json:"state_dir,omitempty"`
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
		WORM: WORMConfig{
			Enabled:       false,
			Mode:          "compliance",
			RetentionDays: 30,
			LegalHold:     false,
		},
		WebDAV: WebDAVConfig{
			Enabled:     false,
			Port:        8089,
			Addr:        "0.0.0.0",
			CacheSizeMB: 196,
			Username:    "",
			Password:    "",
		},
		BotControl: BotControlConfig{
			Enabled:           true,
			PollTimeoutSec:    30,
			AuthorizedUserIDs: []int64{1001, 1002},
			AuthorizedChatIDs: []int64{},
		},
		TelegramAuthorizedUsers: []int64{1001, 1002},
		TelegramAuthorizedChats: []int64{},
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
	if c.WORM.Enabled {
		mode := strings.ToLower(strings.TrimSpace(c.WORM.Mode))
		if mode != "" && mode != "compliance" && mode != "governance" {
			return fmt.Errorf("invalid WORM mode %q: must be 'compliance' or 'governance'", c.WORM.Mode)
		}
		if mode == "compliance" && c.WORM.RetentionDays <= 0 {
			return fmt.Errorf("compliance mode requires retention_days > 0, got %d", c.WORM.RetentionDays)
		}
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
	if wormEn := os.Getenv("WORM_ENABLED"); wormEn != "" {
		cfg.WORM.Enabled = wormEn == "true" || wormEn == "1"
	}
	if wormMode := os.Getenv("WORM_MODE"); wormMode != "" {
		cfg.WORM.Mode = wormMode
	}
	if wormDays := os.Getenv("WORM_RETENTION_DAYS"); wormDays != "" {
		var days int
		if _, err := fmt.Sscanf(wormDays, "%d", &days); err == nil {
			cfg.WORM.RetentionDays = days
		}
	}
	if wormHold := os.Getenv("WORM_LEGAL_HOLD"); wormHold != "" {
		cfg.WORM.LegalHold = wormHold == "true" || wormHold == "1"
	}
	if webdavEn := os.Getenv("WEBDAV_ENABLED"); webdavEn != "" {
		cfg.WebDAV.Enabled = webdavEn == "true" || webdavEn == "1"
	}
	if webdavPort := os.Getenv("WEBDAV_PORT"); webdavPort != "" {
		var port int
		if _, err := fmt.Sscanf(webdavPort, "%d", &port); err == nil && port > 0 {
			cfg.WebDAV.Port = port
		}
	}
	if webdavAddr := os.Getenv("WEBDAV_ADDR"); webdavAddr != "" {
		cfg.WebDAV.Addr = webdavAddr
	}
	if webdavCache := os.Getenv("WEBDAV_CACHE_SIZE_MB"); webdavCache != "" {
		var size int
		if _, err := fmt.Sscanf(webdavCache, "%d", &size); err == nil && size >= 0 {
			cfg.WebDAV.CacheSizeMB = size
		}
	}
	if webdavUser := os.Getenv("WEBDAV_USERNAME"); webdavUser != "" {
		cfg.WebDAV.Username = webdavUser
	}
	if webdavPass := os.Getenv("WEBDAV_PASSWORD"); webdavPass != "" {
		cfg.WebDAV.Password = webdavPass
	}
	if stateDir := os.Getenv("TGDRIVE_STATE_DIR"); stateDir != "" {
		cfg.StateDir = stateDir
	}
	if users := os.Getenv("TELEGRAM_AUTHORIZED_USERS"); users != "" {
		list := parseInt64List(users)
		cfg.TelegramAuthorizedUsers = list
		cfg.BotControl.AuthorizedUserIDs = list
	}
	if chats := os.Getenv("TELEGRAM_AUTHORIZED_CHATS"); chats != "" {
		list := parseInt64List(chats)
		cfg.TelegramAuthorizedChats = list
		cfg.BotControl.AuthorizedChatIDs = list
	}
	if botCtrlEn := os.Getenv("TELEGRAM_BOT_CONTROL_ENABLED"); botCtrlEn != "" {
		cfg.BotControl.Enabled = botCtrlEn == "true" || botCtrlEn == "1"
	}
	if botPollTimeout := os.Getenv("TELEGRAM_BOT_POLL_TIMEOUT"); botPollTimeout != "" {
		var timeout int
		if _, err := fmt.Sscanf(botPollTimeout, "%d", &timeout); err == nil && timeout > 0 {
			cfg.BotControl.PollTimeoutSec = timeout
		}
	}
}

func parseInt64List(s string) []int64 {
	var result []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var val int64
		if _, err := fmt.Sscanf(part, "%d", &val); err == nil {
			result = append(result, val)
		}
	}
	return result
}
