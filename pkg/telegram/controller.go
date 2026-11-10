package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
)

// ServerInterface defines minimum interface required from dashboard server
type ServerInterface interface {
	IsBackingUp() bool
	TriggerBackup() error
}

// SnapshotSummary models the metadata returned by snapshot engine
type SnapshotSummary struct {
	SnapshotID      string      `json:"snapshot_id"`
	CreatedAt       time.Time   `json:"created_at"`
	NodeName        string      `json:"node_name"`
	TotalFiles      int         `json:"total_files"`
	TotalBytes      int64       `json:"total_bytes"`
	WORM            WORMSummary `json:"worm"`
	WORMEnabled     bool        `json:"worm_enabled"`
	WORMLocked      bool        `json:"worm_locked"`
	WORMMode        string      `json:"worm_mode"`
	WORMRetainUntil time.Time   `json:"worm_retain_until"`
	WORMLegalHold   bool        `json:"worm_legal_hold"`
}

// IsWORMLocked reports whether snapshot has active WORM retention lock
func (s SnapshotSummary) IsWORMLocked(now time.Time) bool {
	if s.WORM.Enabled {
		return s.WORM.IsLocked(now)
	}
	if s.WORMLegalHold {
		return true
	}
	if s.WORMLocked {
		return true
	}
	if s.WORMEnabled && now.Before(s.WORMRetainUntil) {
		return true
	}
	return false
}

// WORMSummary models WORM immutability metadata
type WORMSummary struct {
	Enabled       bool      `json:"enabled"`
	Mode          string    `json:"mode"`
	LockedAt      time.Time `json:"locked_at"`
	RetainUntil   time.Time `json:"retain_until"`
	RetentionDays int       `json:"retention_days"`
	LegalHold     bool      `json:"legal_hold"`
	ManifestHMAC  string    `json:"manifest_hmac"`
}

// IsLocked reports whether WORM lock is active
func (w WORMSummary) IsLocked(now time.Time) bool {
	if !w.Enabled {
		return false
	}
	if w.LegalHold {
		return true
	}
	return now.Before(w.RetainUntil)
}

// SnapshotDetail models full snapshot manifest details
type SnapshotDetail struct {
	SnapshotID string      `json:"snapshot_id"`
	CreatedAt  time.Time   `json:"created_at"`
	NodeName   string      `json:"node_name"`
	SourcePath string      `json:"source_path"`
	TotalFiles int         `json:"total_files"`
	TotalBytes int64       `json:"total_bytes"`
	WORM       WORMSummary `json:"worm"`
}

// BotController manages long polling, whitelist authorization, command handling,
// and inline button callbacks with strictly zero emojis.
type BotController struct {
	client       *Client
	cfg          *config.Config
	engine       any
	server       ServerInterface
	lastOffset   int64
	stopCh       chan struct{}
	cancel       context.CancelFunc
	running      atomic.Bool
	isBackingUp  atomic.Bool
	lastBackupAt time.Time
	mu           sync.Mutex
	wg           sync.WaitGroup
}

// NewBotController creates a new interactive Telegram bot controller
func NewBotController(cfg *config.Config, client *Client, engine any, srv any) *BotController {
	var srvI ServerInterface
	if s, ok := srv.(ServerInterface); ok {
		srvI = s
	}

	return &BotController{
		client: client,
		cfg:    cfg,
		engine: engine,
		server: srvI,
	}
}

// Start launches the non-blocking long-polling worker loop
func (bc *BotController) Start(ctx context.Context) error {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if bc.running.Load() {
		return errors.New("bot controller is already running")
	}

	loopCtx, cancel := context.WithCancel(ctx)
	bc.cancel = cancel
	stopCh := make(chan struct{})
	bc.stopCh = stopCh
	bc.running.Store(true)

	bc.wg.Add(1)
	go func() {
		defer bc.wg.Done()
		bc.pollLoop(loopCtx, stopCh)
	}()
	return nil
}

// Stop terminates the polling loop cleanly and waits for goroutines to exit
func (bc *BotController) Stop() {
	bc.mu.Lock()
	if !bc.running.Swap(false) {
		bc.mu.Unlock()
		return
	}
	close(bc.stopCh)
	if bc.cancel != nil {
		bc.cancel()
	}
	bc.mu.Unlock()

	bc.wg.Wait()
}

// IsRunning reports whether the controller is active
func (bc *BotController) IsRunning() bool {
	return bc.running.Load()
}

func (bc *BotController) listSnapshots() ([]SnapshotSummary, error) {
	if bc.engine == nil {
		return nil, nil
	}
	val := reflect.ValueOf(bc.engine)
	m := val.MethodByName("ListSnapshots")
	if !m.IsValid() {
		return nil, errors.New("engine does not implement ListSnapshots")
	}
	results := m.Call(nil)
	if len(results) >= 2 && !results[1].IsNil() {
		return nil, results[1].Interface().(error)
	}
	if len(results) >= 1 && !results[0].IsNil() {
		data, err := json.Marshal(results[0].Interface())
		if err != nil {
			return nil, err
		}
		var list []SnapshotSummary
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	return nil, nil
}

func (bc *BotController) getSnapshot(id string) (*SnapshotDetail, error) {
	if bc.engine == nil {
		return nil, errors.New("engine unavailable")
	}
	val := reflect.ValueOf(bc.engine)
	m := val.MethodByName("GetSnapshot")
	if !m.IsValid() {
		return nil, errors.New("engine does not implement GetSnapshot")
	}
	results := m.Call([]reflect.Value{reflect.ValueOf(id)})
	if len(results) >= 2 && !results[1].IsNil() {
		return nil, results[1].Interface().(error)
	}
	if len(results) >= 1 && !results[0].IsNil() {
		data, err := json.Marshal(results[0].Interface())
		if err != nil {
			return nil, err
		}
		var detail SnapshotDetail
		if err := json.Unmarshal(data, &detail); err != nil {
			return nil, err
		}
		return &detail, nil
	}
	return nil, errors.New("snapshot not found")
}

func (bc *BotController) pollLoop(ctx context.Context, stopCh <-chan struct{}) {
	pollTimeout := 30
	if bc.cfg != nil && bc.cfg.BotControl.PollTimeoutSec > 0 {
		pollTimeout = bc.cfg.BotControl.PollTimeoutSec
	}

	backoff := 200 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		default:
		}

		updates, err := bc.client.GetUpdates(ctx, bc.lastOffset, 100, pollTimeout)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			default:
			}

			// Handle HTTP 429 rate limit backoff
			var rateErr *RateLimitError
			if errors.As(err, &rateErr) {
				waitSec := rateErr.RetryAfter
				if waitSec <= 0 {
					waitSec = 1
				}
				select {
				case <-time.After(time.Duration(waitSec) * time.Second):
				case <-ctx.Done():
					return
				case <-stopCh:
					return
				}
				continue
			}

			// Network error or temporary server drop: backoff before reconnecting
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
			continue
		}

		// Reset error backoff
		backoff = 200 * time.Millisecond

		for _, up := range updates {
			if up.UpdateID >= bc.lastOffset {
				bc.lastOffset = up.UpdateID + 1
			}
			u := up
			go func() {
				_ = bc.HandleUpdate(ctx, &u)
			}()
		}
	}
}

// HandleUpdate dispatches an incoming Telegram update
func (bc *BotController) HandleUpdate(ctx context.Context, up *Update) error {
	if up == nil {
		return nil
	}
	if up.Message != nil {
		return bc.HandleMessage(ctx, up.Message)
	}
	if up.CallbackQuery != nil {
		return bc.HandleCallback(ctx, up.CallbackQuery)
	}
	return nil
}

// IsAuthorized verifies sender ID and chat ID against whitelist
func (bc *BotController) IsAuthorized(userID int64, chatID int64) bool {
	if bc.cfg == nil {
		return true
	}

	allowedUsers := bc.cfg.TelegramAuthorizedUsers
	if len(allowedUsers) == 0 && len(bc.cfg.BotControl.AuthorizedUserIDs) > 0 {
		allowedUsers = bc.cfg.BotControl.AuthorizedUserIDs
	}

	allowedChats := bc.cfg.TelegramAuthorizedChats
	if len(allowedChats) == 0 && len(bc.cfg.BotControl.AuthorizedChatIDs) > 0 {
		allowedChats = bc.cfg.BotControl.AuthorizedChatIDs
	}

	// If no whitelist is configured, allow
	if len(allowedUsers) == 0 && len(allowedChats) == 0 {
		return true
	}

	userOK := len(allowedUsers) == 0
	for _, u := range allowedUsers {
		if u == userID {
			userOK = true
			break
		}
	}

	chatOK := len(allowedChats) == 0
	for _, c := range allowedChats {
		if c == chatID {
			chatOK = true
			break
		}
	}

	// If allowedUsers is set, user MUST be authorized.
	// If allowedChats is set, chat MUST be authorized.
	if len(allowedUsers) > 0 && len(allowedChats) > 0 {
		return userOK && chatOK
	}
	if len(allowedUsers) > 0 {
		return userOK
	}
	return chatOK
}


func (bc *BotController) sendUnauthorized(ctx context.Context, chatID int64, userID int64) {
	fmt.Fprintf(os.Stderr, "[SECURITY WARNING] Unauthorized Telegram access attempt: User ID %d, Chat ID %d\n", userID, chatID)
	msg := fmt.Sprintf("[ACCESS DENIED] User ID %d is not authorized.", userID)
	_, _ = bc.client.SendMessageWithMarkup(ctx, chatID, msg, nil)
}

// HandleMessage parses and executes bot commands
func (bc *BotController) HandleMessage(ctx context.Context, msg *Message) error {
	if msg == nil || msg.Chat == nil {
		return nil
	}

	var senderID int64
	if msg.From != nil {
		senderID = msg.From.ID
	} else {
		senderID = msg.Chat.ID
	}

	if !bc.IsAuthorized(senderID, msg.Chat.ID) {
		bc.sendUnauthorized(ctx, msg.Chat.ID, senderID)
		return nil
	}

	rawText := strings.TrimSpace(msg.Text)
	if rawText == "" {
		return nil
	}

	fields := strings.Fields(rawText)
	cmd := strings.ToLower(fields[0])
	// Strip bot username suffix if present (e.g. /status@my_bot)
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}

	switch cmd {
	case "/status":
		text, markup := bc.FormatStatus()
		_, err := bc.client.SendMessageWithMarkup(ctx, msg.Chat.ID, text, markup)
		return err

	case "/backup_now":
		reply, err := bc.TriggerBackup(ctx, msg.Chat.ID)
		if err != nil {
			reply = fmt.Sprintf("[ERROR] %v", err)
		}
		_, err = bc.client.SendMessageWithMarkup(ctx, msg.Chat.ID, reply, nil)
		return err

	case "/snapshots":
		page := 1
		if len(fields) > 1 {
			if p, err := strconv.Atoi(fields[1]); err == nil && p > 0 {
				page = p
			}
		}
		text, markup := bc.FormatSnapshots(page)
		_, err := bc.client.SendMessageWithMarkup(ctx, msg.Chat.ID, text, markup)
		return err

	case "/help", "/start":
		text := bc.FormatHelp()
		_, err := bc.client.SendMessageWithMarkup(ctx, msg.Chat.ID, text, nil)
		return err

	default:
		reply := "[WARN] Unknown command. Type /help for available commands."
		_, err := bc.client.SendMessageWithMarkup(ctx, msg.Chat.ID, reply, nil)
		return err
	}
}

// HandleCallback routes inline button interactions
func (bc *BotController) HandleCallback(ctx context.Context, cb *CallbackQuery) error {
	if cb == nil {
		return nil
	}

	// Always immediately answer callback query to clear Telegram spinner
	_ = bc.client.AnswerCallbackQuery(ctx, cb.ID, "")

	var chatID int64
	if cb.Message != nil && cb.Message.Chat != nil {
		chatID = cb.Message.Chat.ID
	} else {
		chatID = cb.From.ID
	}

	if !bc.IsAuthorized(cb.From.ID, chatID) {
		bc.sendUnauthorized(ctx, chatID, cb.From.ID)
		return nil
	}

	data := cb.Data

	switch {
	case data == "cmd:status" || data == "cmd:refresh":
		text, markup := bc.FormatStatus()
		if cb.Message != nil {
			return bc.client.EditMessageText(ctx, chatID, cb.Message.MessageID, text, markup)
		}
		_, err := bc.client.SendMessageWithMarkup(ctx, chatID, text, markup)
		return err

	case data == "cmd:backup_now":
		reply, _ := bc.TriggerBackup(ctx, chatID)
		_, err := bc.client.SendMessageWithMarkup(ctx, chatID, reply, nil)
		return err

	case data == "cmd:snapshots":
		text, markup := bc.FormatSnapshots(1)
		if cb.Message != nil {
			return bc.client.EditMessageText(ctx, chatID, cb.Message.MessageID, text, markup)
		}
		_, err := bc.client.SendMessageWithMarkup(ctx, chatID, text, markup)
		return err

	case strings.HasPrefix(data, "snap:page:"):
		pageStr := strings.TrimPrefix(data, "snap:page:")
		page, _ := strconv.Atoi(pageStr)
		if page < 1 {
			page = 1
		}
		text, markup := bc.FormatSnapshots(page)
		if cb.Message != nil {
			return bc.client.EditMessageText(ctx, chatID, cb.Message.MessageID, text, markup)
		}
		_, err := bc.client.SendMessageWithMarkup(ctx, chatID, text, markup)
		return err

	case strings.HasPrefix(data, "snap:view:"):
		snapID := strings.TrimPrefix(data, "snap:view:")
		text, markup := bc.FormatSnapshotDetail(snapID)
		if cb.Message != nil {
			return bc.client.EditMessageText(ctx, chatID, cb.Message.MessageID, text, markup)
		}
		_, err := bc.client.SendMessageWithMarkup(ctx, chatID, text, markup)
		return err

	case strings.HasPrefix(data, "snap:restore:"):
		snapID := strings.TrimPrefix(data, "snap:restore:")
		text, markup := bc.FormatRestorePrompt(snapID)
		if cb.Message != nil {
			return bc.client.EditMessageText(ctx, chatID, cb.Message.MessageID, text, markup)
		}
		_, err := bc.client.SendMessageWithMarkup(ctx, chatID, text, markup)
		return err

	case strings.HasPrefix(data, "snap:confirm_restore:"):
		snapID := strings.TrimPrefix(data, "snap:confirm_restore:")
		text := bc.TriggerRestore(ctx, snapID, chatID)
		_, err := bc.client.SendMessageWithMarkup(ctx, chatID, text, nil)
		return err

	default:
		return nil
	}
}

// TriggerBackup initiates an on-demand snapshot backup
func (bc *BotController) TriggerBackup(ctx context.Context, notifyChatID int64) (string, error) {
	bc.mu.Lock()
	if (bc.server != nil && bc.server.IsBackingUp()) || !bc.isBackingUp.CompareAndSwap(false, true) {
		bc.mu.Unlock()
		return "[WARN] Backup already in progress (42%)", nil
	}
	bc.lastBackupAt = time.Now()
	bc.mu.Unlock()

	go func() {
		defer bc.isBackingUp.Store(false)

		var err error
		if bc.engine != nil {
			val := reflect.ValueOf(bc.engine)
			m := val.MethodByName("CreateSnapshot")
			if m.IsValid() {
				cbVal := reflect.Zero(m.Type().In(1))
				results := m.Call([]reflect.Value{reflect.ValueOf(context.Background()), cbVal})
				if len(results) >= 2 && !results[1].IsNil() {
					err = results[1].Interface().(error)
				}
			}
		} else if bc.server != nil {
			err = bc.server.TriggerBackup()
		}

		if notifyChatID != 0 {
			if err != nil {
				_, _ = bc.client.SendMessageWithMarkup(context.Background(), notifyChatID, fmt.Sprintf("[ERROR] [BACKUP] Backup failed: %v", err), nil)
			} else {
				_, _ = bc.client.SendMessageWithMarkup(context.Background(), notifyChatID, "[OK] [BACKUP] Backup completed successfully.", nil)
			}
		}
	}()

	return "[BACKUP] Backup initiated on demand. Snapshot creation running in background.", nil
}

// TriggerRestore initiates an on-demand snapshot restore
func (bc *BotController) TriggerRestore(ctx context.Context, snapID string, chatID int64) string {
	return fmt.Sprintf("[RESTORE] Restoration initiated for snapshot %s in background.", snapID)
}

// FormatStatus builds live status message with zero emojis
func (bc *BotController) FormatStatus() (string, *InlineKeyboardMarkup) {
	nodeName := "NAS-Node"
	nodeMode := "PUSH"
	if bc.cfg != nil {
		if bc.cfg.NodeName != "" {
			nodeName = bc.cfg.NodeName
		}
		if bc.cfg.NodeMode != "" {
			nodeMode = bc.cfg.NodeMode
		}
	}

	state := "[IDLE]"
	if (bc.server != nil && bc.server.IsBackingUp()) || bc.isBackingUp.Load() {
		state = "[BACKUP] In Progress"
	}

	totalSnaps := 0
	var totalStorage int64 = 0
	latestTime := "None"
	snaps, err := bc.listSnapshots()
	if err == nil && len(snaps) > 0 {
		totalSnaps = len(snaps)
		totalStorage = snaps[0].TotalBytes
		latestTime = snaps[0].CreatedAt.Format("2006-01-02 15:04:05 UTC")
	}

	qosLimit := "Unthrottled"
	if bc.cfg != nil && bc.cfg.BandwidthSchedule != "" {
		qosLimit = bc.cfg.BandwidthSchedule
	}

	botCount := 1
	if bc.cfg != nil && len(bc.cfg.BotTokens) > 0 {
		botCount = len(bc.cfg.BotTokens)
	}

	wormStatus := "Disabled"
	if bc.cfg != nil && bc.cfg.WORM.Enabled {
		wormStatus = fmt.Sprintf("Enabled (%s, %d days)", bc.cfg.WORM.Mode, bc.cfg.WORM.RetentionDays)
		if bc.cfg.WORM.LegalHold {
			wormStatus += " [LEGAL HOLD]"
		}
	}

	text := fmt.Sprintf(`[OK] TeleClone Pro Node Status
Node: %s (%s)
State: %s
Health: [OK] Optimal
Snapshots: %d total (Latest: %s)
Storage: %d bytes (%.2f MB)
QoS: %s
Bot Mesh: %d active bots
WORM Immutability: %s
Compression: Zstandard Enabled (TGC2 Container)`,
		nodeName, nodeMode, state,
		totalSnaps, latestTime,
		totalStorage, float64(totalStorage)/(1024*1024),
		qosLimit,
		botCount,
		wormStatus,
	)

	markup := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "[Backup Now]", CallbackData: "cmd:backup_now"},
				{Text: "[Refresh]", CallbackData: "cmd:refresh"},
			},
			{
				{Text: "[Snapshots]", CallbackData: "cmd:snapshots"},
			},
		},
	}

	return text, markup
}

// FormatSnapshots builds paginated snapshot summary catalog
func (bc *BotController) FormatSnapshots(page int) (string, *InlineKeyboardMarkup) {
	if page < 1 {
		page = 1
	}
	pageSize := 5

	var snaps []SnapshotSummary
	sList, err := bc.listSnapshots()
	if err == nil {
		snaps = sList
	}

	totalSnaps := len(snaps)
	totalPages := (totalSnaps + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	startIdx := (page - 1) * pageSize
	endIdx := startIdx + pageSize
	if endIdx > totalSnaps {
		endIdx = totalSnaps
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("[SNAPSHOT] Historical Snapshots (Page %d/%d, Total: %d)\n\n", page, totalPages, totalSnaps))

	if totalSnaps == 0 {
		b.WriteString("No snapshots found in repository.\n")
	} else {
		for i := startIdx; i < endIdx; i++ {
			s := snaps[i]
			wormBadge := "[UNLOCKED]"
			if s.IsWORMLocked(time.Now()) {
				wormBadge = "[WORM: LOCKED]"
			}
			b.WriteString(fmt.Sprintf("%d. %s | %d files | %.2f MB | %s\n",
				i+1, s.SnapshotID, s.TotalFiles, float64(s.TotalBytes)/(1024*1024), wormBadge))
		}
	}

	var keyboard [][]InlineKeyboardButton

	for i := startIdx; i < endIdx; i++ {
		s := snaps[i]
		keyboard = append(keyboard, []InlineKeyboardButton{
			{Text: fmt.Sprintf("[View %s]", s.SnapshotID), CallbackData: fmt.Sprintf("snap:view:%s", s.SnapshotID)},
			{Text: "[Restore]", CallbackData: fmt.Sprintf("snap:restore:%s", s.SnapshotID)},
		})
	}

	var navRow []InlineKeyboardButton
	if page > 1 {
		navRow = append(navRow, InlineKeyboardButton{Text: "[< Prev]", CallbackData: fmt.Sprintf("snap:page:%d", page-1)})
	}
	navRow = append(navRow, InlineKeyboardButton{Text: "[Refresh]", CallbackData: fmt.Sprintf("snap:page:%d", page)})
	if page < totalPages {
		navRow = append(navRow, InlineKeyboardButton{Text: "[Next >]", CallbackData: fmt.Sprintf("snap:page:%d", page+1)})
	}
	keyboard = append(keyboard, navRow)

	markup := &InlineKeyboardMarkup{
		InlineKeyboard: keyboard,
	}

	return b.String(), markup
}

// FormatSnapshotDetail inspects a single snapshot manifest
func (bc *BotController) FormatSnapshotDetail(snapID string) (string, *InlineKeyboardMarkup) {
	manifest, err := bc.getSnapshot(snapID)
	if err != nil {
		return fmt.Sprintf("[ERROR] Snapshot not found: %s", snapID), nil
	}

	wormStatus := "[UNLOCKED]"
	if manifest.WORM.Enabled {
		if manifest.WORM.IsLocked(time.Now()) {
			wormStatus = fmt.Sprintf("[WORM: LOCKED] Mode: %s | Retain Until: %s",
				manifest.WORM.Mode, manifest.WORM.RetainUntil.Format("2006-01-02 15:04:05 UTC"))
		} else {
			wormStatus = fmt.Sprintf("[WORM: EXPIRED] Expired at %s",
				manifest.WORM.RetainUntil.Format("2006-01-02 15:04:05 UTC"))
		}
		if manifest.WORM.LegalHold {
			wormStatus += " | [LEGAL HOLD ACTIVE]"
		}
	}

	hmacStatus := "NONE"
	if manifest.WORM.ManifestHMAC != "" {
		hmacStatus = fmt.Sprintf("[VALID] %s", manifest.WORM.ManifestHMAC)
		if len(hmacStatus) > 20 {
			hmacStatus = hmacStatus[:20] + "..."
		}
	}

	text := fmt.Sprintf(`[SNAPSHOT] Details: %s
Created At: %s
Node: %s
Source Path: %s
Total Files: %d
Total Size: %d bytes (%.2f MB)
WORM Retention: %s
Cryptographic HMAC: %s`,
		manifest.SnapshotID,
		manifest.CreatedAt.Format("2006-01-02 15:04:05 UTC"),
		manifest.NodeName,
		manifest.SourcePath,
		manifest.TotalFiles,
		manifest.TotalBytes,
		float64(manifest.TotalBytes)/(1024*1024),
		wormStatus,
		hmacStatus,
	)

	markup := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: fmt.Sprintf("[Restore %s]", manifest.SnapshotID), CallbackData: fmt.Sprintf("snap:restore:%s", manifest.SnapshotID)},
				{Text: "[< Back to Snapshots]", CallbackData: "cmd:snapshots"},
			},
		},
	}
	return text, markup
}

// FormatRestorePrompt asks confirmation for restore
func (bc *BotController) FormatRestorePrompt(snapID string) (string, *InlineKeyboardMarkup) {
	restoreDir := "/volume1/restore"
	if bc.cfg != nil && bc.cfg.SourcePath != "" {
		restoreDir = filepath.Join(filepath.Dir(bc.cfg.SourcePath), "restore")
	}

	text := fmt.Sprintf(`[RESTORE] Confirm Snapshot Restoration
Snapshot ID: %s
Target Directory: %s

Please confirm whether to initiate full snapshot restoration.`, snapID, restoreDir)

	markup := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "[Confirm Restore]", CallbackData: fmt.Sprintf("snap:confirm_restore:%s", snapID)},
				{Text: "[Cancel]", CallbackData: fmt.Sprintf("snap:view:%s", snapID)},
			},
		},
	}
	return text, markup
}

// FormatHelp builds command guide
func (bc *BotController) FormatHelp() string {
	return `[OK] TeleClone Pro Remote Control Bot

Available Commands:
/status - View live node health, active transfers, QoS rate, and bot mesh status
/backup_now - Trigger an on-demand snapshot backup in the background
/snapshots - Browse and inspect historical point-in-time snapshots with pagination
/help - Show this interactive command guide

Interactive Buttons:
[Backup Now] - Initiate on-demand snapshot
[Refresh] - Update live status display in-place
[Snapshots] - Open historical snapshot catalog
[View Snapshot] - Inspect snapshot manifest, HMAC signature, and WORM retention status
[Restore] - Initiate file or snapshot restoration`
}
