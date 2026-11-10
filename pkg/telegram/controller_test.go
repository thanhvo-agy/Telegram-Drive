package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
)

// mockTelegramServer simulates Telegram Bot API for unit tests
type mockTelegramServer struct {
	server       *httptest.Server
	mu           sync.Mutex
	updates      []Update
	sentMessages []Message
	editedText   []string
	answeredCBs  []string
	lastOffset   int64
	rateLimitSec int
	dropNext     atomic.Bool
	updateID     int64
	msgID        int64
}

func newMockTelegramServer() *mockTelegramServer {
	m := &mockTelegramServer{
		updateID: 100,
		msgID:    200,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/botmock_tok/getUpdates", m.handleGetUpdates)
	mux.HandleFunc("/botmock_tok/sendMessage", m.handleSendMessage)
	mux.HandleFunc("/botmock_tok/editMessageText", m.handleEditMessageText)
	mux.HandleFunc("/botmock_tok/answerCallbackQuery", m.handleAnswerCallbackQuery)
	mux.HandleFunc("/botmock_tok/getMe", m.handleGetMe)

	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockTelegramServer) URL() string {
	return m.server.URL
}

func (m *mockTelegramServer) Close() {
	m.server.Close()
}

func (m *mockTelegramServer) handleGetMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true,
		"result": map[string]interface{}{
			"id":         123456,
			"is_bot":     true,
			"first_name": "TestBot",
			"username":   "test_drive_bot",
		},
	})
}

func (m *mockTelegramServer) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	if m.dropNext.CompareAndSwap(true, false) {
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			if conn != nil {
				_ = conn.Close()
				return
			}
		}
		http.Error(w, "connection dropped", http.StatusBadGateway)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.rateLimitSec > 0 {
		sec := m.rateLimitSec
		m.rateLimitSec = 0
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":          false,
			"error_code":  429,
			"description": "Too Many Requests: retry after " + strconv.Itoa(sec),
			"parameters":  map[string]interface{}{"retry_after": sec},
		})
		return
	}

	offsetStr := r.URL.Query().Get("offset")
	var offset int64
	if offsetStr != "" {
		offset, _ = strconv.ParseInt(offsetStr, 10, 64)
	}
	m.lastOffset = offset

	var pending []Update
	for _, u := range m.updates {
		if u.UpdateID >= offset {
			pending = append(pending, u)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"result": pending,
	})
}

func (m *mockTelegramServer) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	chatID, _ := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
	text := r.Form.Get("text")

	m.mu.Lock()
	m.msgID++
	msg := Message{
		MessageID: m.msgID,
		Date:      time.Now().Unix(),
		Text:      text,
		Chat:      &Chat{ID: chatID, Type: "private"},
	}
	m.sentMessages = append(m.sentMessages, msg)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"result": msg,
	})
}

func (m *mockTelegramServer) handleEditMessageText(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	text := r.Form.Get("text")

	m.mu.Lock()
	m.editedText = append(m.editedText, text)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"result": true,
	})
}

func (m *mockTelegramServer) handleAnswerCallbackQuery(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	cbID := r.Form.Get("callback_query_id")

	m.mu.Lock()
	m.answeredCBs = append(m.answeredCBs, cbID)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"result": true,
	})
}

func (m *mockTelegramServer) injectMessage(userID int64, chatID int64, text string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.updateID++
	m.msgID++
	up := Update{
		UpdateID: m.updateID,
		Message: &Message{
			MessageID: m.msgID,
			From:      &User{ID: userID, FirstName: "User"},
			Chat:      &Chat{ID: chatID, Type: "private"},
			Date:      time.Now().Unix(),
			Text:      text,
		},
	}
	m.updates = append(m.updates, up)
}

func (m *mockTelegramServer) injectCallback(userID int64, chatID int64, origMsgID int64, data string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.updateID++
	cbID := fmt.Sprintf("cb_%d", time.Now().UnixNano())
	up := Update{
		UpdateID: m.updateID,
		CallbackQuery: &CallbackQuery{
			ID:   cbID,
			From: User{ID: userID, FirstName: "User"},
			Message: &Message{
				MessageID: origMsgID,
				Chat:      &Chat{ID: chatID, Type: "private"},
			},
			Data: data,
		},
	}
	m.updates = append(m.updates, up)
	return cbID
}

func (m *mockTelegramServer) getLastSentMessage() (Message, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sentMessages) == 0 {
		return Message{}, false
	}
	return m.sentMessages[len(m.sentMessages)-1], true
}

func (m *mockTelegramServer) getLastEditedText() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.editedText) == 0 {
		return "", false
	}
	return m.editedText[len(m.editedText)-1], true
}

// mockEngine implements mock snapshot list and detail
type mockEngine struct {
	snapshots []SnapshotSummary
	details   map[string]*SnapshotDetail
	isBacking atomic.Bool
}

func (me *mockEngine) ListSnapshots() ([]SnapshotSummary, error) {
	return me.snapshots, nil
}

func (me *mockEngine) GetSnapshot(id string) (*SnapshotDetail, error) {
	if d, ok := me.details[id]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("snapshot %s not found", id)
}

func (me *mockEngine) CreateSnapshot(ctx context.Context, cb any) (*SnapshotDetail, error) {
	time.Sleep(10 * time.Millisecond)
	return &SnapshotDetail{
		SnapshotID: "snap-new-001",
		CreatedAt:  time.Now(),
		TotalFiles: 5,
		TotalBytes: 50 * 1024 * 1024,
	}, nil
}

func isEmoji(r rune) bool {
	if r < 0x2000 {
		return false
	}
	if (r >= 0x1F300 && r <= 0x1F5FF) ||
		(r >= 0x1F600 && r <= 0x1F64F) ||
		(r >= 0x1F680 && r <= 0x1F6FF) ||
		(r >= 0x1F900 && r <= 0x1F9FF) ||
		(r >= 0x1FA70 && r <= 0x1FAFF) ||
		(r >= 0x2600 && r <= 0x26FF) ||
		(r >= 0x2700 && r <= 0x27BF) {
		return true
	}
	switch r {
	case 0x203C, 0x2049, 0x2122, 0x2139, 0x2194, 0x2195, 0x2196, 0x2197, 0x2198, 0x2199, 0x21A9, 0x21AA,
		0x231A, 0x231B, 0x2328, 0x23CF, 0x23E9, 0x23EA, 0x23EB, 0x23EC, 0x23ED, 0x23EE, 0x23EF, 0x23F0,
		0x23F1, 0x23F2, 0x23F3, 0x23F8, 0x23F9, 0x23FA, 0x24C2, 0x25AA, 0x25AB, 0x25B6, 0x25C0, 0x25FB,
		0x25FC, 0x25FD, 0x25FE, 0x2934, 0x2935, 0x2B05, 0x2B06, 0x2B07, 0x2B1B, 0x2B1C, 0x2B50, 0x2B55:
		return true
	}
	return false
}

func checkZeroEmojis(t *testing.T, text string) {
	for _, r := range text {
		if isEmoji(r) {
			t.Fatalf("EMOJI VIOLATION DETECTED: %c (U+%04X) in %q", r, r, text)
		}
	}
}

func setupTestBot(t *testing.T) (*mockTelegramServer, *BotController, *Client) {
	mockTG := newMockTelegramServer()
	t.Cleanup(mockTG.Close)

	cfg := config.DefaultConfig()
	cfg.BotTokens = []string{"mock_tok"}
	cfg.ChannelID = "-1001999888777"
	cfg.TelegramAuthorizedUsers = []int64{1001, 1002}
	cfg.BotControl.AuthorizedUserIDs = []int64{1001, 1002}
	cfg.BotControl.PollTimeoutSec = 1

	client, err := NewClient(cfg.BotTokens, cfg.ChannelID, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURL(mockTG.URL())

	engine := &mockEngine{
		snapshots: []SnapshotSummary{
			{
				SnapshotID: "snap-20261110-100000",
				CreatedAt:  time.Now().Add(-1 * time.Hour),
				NodeName:   "NAS-Node",
				TotalFiles: 42,
				TotalBytes: 104857600,
				WORM: WORMSummary{
					Enabled:       true,
					Mode:          "compliance",
					RetainUntil:   time.Now().Add(30 * 24 * time.Hour),
					RetentionDays: 30,
					ManifestHMAC:  "a1b2c3d4e5f60718293a",
				},
			},
			{
				SnapshotID: "snap-20261109-100000",
				CreatedAt:  time.Now().Add(-25 * time.Hour),
				NodeName:   "NAS-Node",
				TotalFiles: 35,
				TotalBytes: 90000000,
				WORM: WORMSummary{
					Enabled: false,
				},
			},
		},
		details: map[string]*SnapshotDetail{
			"snap-20261110-100000": {
				SnapshotID: "snap-20261110-100000",
				CreatedAt:  time.Now().Add(-1 * time.Hour),
				NodeName:   "NAS-Node",
				SourcePath: "/volume1/data",
				TotalFiles: 42,
				TotalBytes: 104857600,
				WORM: WORMSummary{
					Enabled:       true,
					Mode:          "compliance",
					RetainUntil:   time.Now().Add(30 * 24 * time.Hour),
					RetentionDays: 30,
					ManifestHMAC:  "a1b2c3d4e5f60718293a",
				},
			},
		},
	}

	ctrl := NewBotController(cfg, client, engine, nil)
	return mockTG, ctrl, client
}

func TestController_Status_Command(t *testing.T) {
	mockTG, ctrl, _ := setupTestBot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	mockTG.injectMessage(1001, 1001, "/status")
	time.Sleep(100 * time.Millisecond)

	msg, found := mockTG.getLastSentMessage()
	if !found {
		t.Fatalf("expected message to be sent")
	}

	checkZeroEmojis(t, msg.Text)

	if !strings.Contains(msg.Text, "[OK]") || !strings.Contains(msg.Text, "Node:") {
		t.Errorf("status missing required badge or node info: %s", msg.Text)
	}
	if !strings.Contains(msg.Text, "Snapshots:") || !strings.Contains(msg.Text, "Storage:") || !strings.Contains(msg.Text, "QoS:") {
		t.Errorf("status missing metrics tokens: %s", msg.Text)
	}
}

func TestController_BackupNow_Command(t *testing.T) {
	mockTG, ctrl, _ := setupTestBot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	mockTG.injectMessage(1001, 1001, "/backup_now")
	time.Sleep(100 * time.Millisecond)

	msg, found := mockTG.getLastSentMessage()
	if !found {
		t.Fatalf("expected message to be sent")
	}

	checkZeroEmojis(t, msg.Text)

	if !strings.Contains(msg.Text, "Backup") && !strings.Contains(msg.Text, "[BACKUP]") {
		t.Errorf("backup_now response missing confirmation: %s", msg.Text)
	}
}

func TestController_Snapshots_Command_And_Pagination(t *testing.T) {
	mockTG, ctrl, _ := setupTestBot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	mockTG.injectMessage(1001, 1001, "/snapshots")
	time.Sleep(100 * time.Millisecond)

	msg, found := mockTG.getLastSentMessage()
	if !found {
		t.Fatalf("expected message to be sent")
	}

	checkZeroEmojis(t, msg.Text)

	if !strings.Contains(msg.Text, "[SNAPSHOT]") || !strings.Contains(msg.Text, "snap-20261110-100000") {
		t.Errorf("snapshots response missing snapshot info: %s", msg.Text)
	}
}

func TestController_Help_Command(t *testing.T) {
	mockTG, ctrl, _ := setupTestBot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	mockTG.injectMessage(1001, 1001, "/help")
	time.Sleep(100 * time.Millisecond)

	msg, found := mockTG.getLastSentMessage()
	if !found {
		t.Fatalf("expected message to be sent")
	}

	checkZeroEmojis(t, msg.Text)

	for _, cmd := range []string{"/status", "/backup_now", "/snapshots", "/help"} {
		if !strings.Contains(msg.Text, cmd) {
			t.Errorf("help response missing command %s: %s", cmd, msg.Text)
		}
	}
}

func TestController_Whitelist_Rejection(t *testing.T) {
	mockTG, ctrl, _ := setupTestBot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	// Unauthorized user ID 8888888
	mockTG.injectMessage(8888888, 8888888, "/backup_now")
	time.Sleep(100 * time.Millisecond)

	msg, found := mockTG.getLastSentMessage()
	if !found {
		t.Fatalf("expected rejection message to be sent")
	}

	checkZeroEmojis(t, msg.Text)

	if !strings.Contains(msg.Text, "ACCESS DENIED") && !strings.Contains(msg.Text, "Unauthorized") {
		t.Errorf("expected access denied message, got: %s", msg.Text)
	}
}

func TestController_CallbackQuery_Routing(t *testing.T) {
	mockTG, ctrl, _ := setupTestBot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	// 1. Callback: cmd:refresh
	mockTG.injectCallback(1001, 1001, 501, "cmd:refresh")
	time.Sleep(100 * time.Millisecond)

	edited, found := mockTG.getLastEditedText()
	if !found {
		t.Fatalf("expected message to be edited via cmd:refresh")
	}
	checkZeroEmojis(t, edited)
	if !strings.Contains(edited, "[OK]") || !strings.Contains(edited, "Node:") {
		t.Errorf("edited status text malformed: %s", edited)
	}

	// 2. Callback: snap:view
	mockTG.injectCallback(1001, 1001, 501, "snap:view:snap-20261110-100000")
	time.Sleep(100 * time.Millisecond)

	edited2, found2 := mockTG.getLastEditedText()
	if !found2 {
		t.Fatalf("expected message to be edited via snap:view")
	}
	checkZeroEmojis(t, edited2)
	if !strings.Contains(edited2, "snap-20261110-100000") || !strings.Contains(edited2, "[WORM: LOCKED]") {
		t.Errorf("snapshot detail malformed: %s", edited2)
	}

	// 3. Callback: snap:restore
	mockTG.injectCallback(1001, 1001, 501, "snap:restore:snap-20261110-100000")
	time.Sleep(100 * time.Millisecond)

	edited3, found3 := mockTG.getLastEditedText()
	if !found3 {
		t.Fatalf("expected message to be edited via snap:restore")
	}
	checkZeroEmojis(t, edited3)
	if !strings.Contains(edited3, "[RESTORE]") {
		t.Errorf("restore prompt malformed: %s", edited3)
	}
}

func TestController_Concurrent_Rapid_Commands_Race(t *testing.T) {
	mockTG, ctrl, _ := setupTestBot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	var wg sync.WaitGroup
	commands := []string{"/status", "/backup_now", "/snapshots", "/help", "unknown"}

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cmd := commands[idx%len(commands)]
			mockTG.injectMessage(1001, 1001, cmd)
		}(i)
	}

	wg.Wait()
	time.Sleep(200 * time.Millisecond)

	if !ctrl.IsRunning() {
		t.Errorf("expected controller to remain running")
	}
}

func TestController_Network_Drop_Recovery(t *testing.T) {
	mockTG, ctrl, _ := setupTestBot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	// Simulate socket drop
	mockTG.dropNext.Store(true)

	// Inject a command to be processed after drop
	mockTG.injectMessage(1001, 1001, "/status")
	time.Sleep(400 * time.Millisecond)

	msg, found := mockTG.getLastSentMessage()
	if !found {
		t.Fatalf("expected message to be sent after auto-reconnecting from network drop")
	}

	checkZeroEmojis(t, msg.Text)
	if !strings.Contains(msg.Text, "[OK]") {
		t.Errorf("expected [OK] badge in status after reconnection: %s", msg.Text)
	}
}

func TestEscapeMarkdown_Formatting(t *testing.T) {
	input := "file_[name]*with*symbols.tar.gz"
	escaped := EscapeMarkdown(input)
	expected := "file\\_\\[name\\]\\*with\\*symbols\\.tar\\.gz"
	if escaped != expected {
		t.Fatalf("EscapeMarkdown mismatch: got %q, want %q", escaped, expected)
	}
}

func TestController_IsAuthorized(t *testing.T) {
	// 1. Both allowedUsers and allowedChats configured
	cfgBoth := &config.Config{
		TelegramAuthorizedUsers: []int64{1001, 1002},
		TelegramAuthorizedChats: []int64{-100123456},
	}
	ctrlBoth := NewBotController(cfgBoth, nil, nil, nil)
	if !ctrlBoth.IsAuthorized(1001, -100123456) {
		t.Errorf("expected authorized user in authorized chat to be allowed")
	}
	if ctrlBoth.IsAuthorized(9999, -100123456) {
		t.Errorf("expected unauthorized user in authorized chat to be REJECTED")
	}
	if ctrlBoth.IsAuthorized(1001, -999999) {
		t.Errorf("expected authorized user in unauthorized chat to be REJECTED")
	}
	if ctrlBoth.IsAuthorized(9999, -999999) {
		t.Errorf("expected unauthorized user in unauthorized chat to be REJECTED")
	}

	// 2. Only allowedUsers configured
	cfgUsersOnly := &config.Config{
		TelegramAuthorizedUsers: []int64{1001},
	}
	ctrlUsersOnly := NewBotController(cfgUsersOnly, nil, nil, nil)
	if !ctrlUsersOnly.IsAuthorized(1001, 12345) {
		t.Errorf("expected authorized user to be allowed in any chat")
	}
	if ctrlUsersOnly.IsAuthorized(9999, 12345) {
		t.Errorf("expected unauthorized user to be rejected")
	}

	// 3. Only allowedChats configured
	cfgChatsOnly := &config.Config{
		TelegramAuthorizedChats: []int64{-100123456},
	}
	ctrlChatsOnly := NewBotController(cfgChatsOnly, nil, nil, nil)
	if !ctrlChatsOnly.IsAuthorized(9999, -100123456) {
		t.Errorf("expected any user in authorized chat to be allowed when allowedUsers is empty")
	}
	if ctrlChatsOnly.IsAuthorized(9999, -999999) {
		t.Errorf("expected user in unauthorized chat to be rejected")
	}

	// 4. Neither configured (open whitelist)
	cfgEmpty := &config.Config{}
	ctrlEmpty := NewBotController(cfgEmpty, nil, nil, nil)
	if !ctrlEmpty.IsAuthorized(9999, 8888) {
		t.Errorf("expected any user in any chat to be allowed when whitelist is empty")
	}

	// 5. Fallback to BotControl struct
	cfgBotControl := &config.Config{}
	cfgBotControl.BotControl.AuthorizedUserIDs = []int64{5001}
	cfgBotControl.BotControl.AuthorizedChatIDs = []int64{-5002}
	ctrlBotControl := NewBotController(cfgBotControl, nil, nil, nil)
	if !ctrlBotControl.IsAuthorized(5001, -5002) {
		t.Errorf("expected authorized user and chat via BotControl config to be allowed")
	}
	if ctrlBotControl.IsAuthorized(9999, -5002) {
		t.Errorf("expected unauthorized user in authorized chat via BotControl config to be rejected")
	}

	// 6. Nil config
	ctrlNil := NewBotController(nil, nil, nil, nil)
	if !ctrlNil.IsAuthorized(9999, 8888) {
		t.Errorf("expected true when cfg is nil")
	}
}

func TestController_Whitelist_UnauthorizedUserInAuthorizedChat(t *testing.T) {
	mockTG := newMockTelegramServer()
	t.Cleanup(mockTG.Close)

	cfg := config.DefaultConfig()
	cfg.BotTokens = []string{"mock_tok"}
	cfg.ChannelID = "-1001999888777"
	cfg.TelegramAuthorizedUsers = []int64{1001, 1002}
	cfg.TelegramAuthorizedChats = []int64{-1001999888777}
	cfg.BotControl.AuthorizedUserIDs = []int64{1001, 1002}
	cfg.BotControl.AuthorizedChatIDs = []int64{-1001999888777}
	cfg.BotControl.PollTimeoutSec = 1

	client, err := NewClient(cfg.BotTokens, cfg.ChannelID, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURL(mockTG.URL())

	engine := &mockEngine{}
	ctrl := NewBotController(cfg, client, engine, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	// Unauthorized user ID 9999999 in authorized group chat -1001999888777
	mockTG.injectMessage(9999999, -1001999888777, "/backup_now")
	time.Sleep(100 * time.Millisecond)

	msg, found := mockTG.getLastSentMessage()
	if !found {
		t.Fatalf("expected rejection message to be sent")
	}

	checkZeroEmojis(t, msg.Text)

	if !strings.Contains(msg.Text, "ACCESS DENIED") && !strings.Contains(msg.Text, "Unauthorized") {
		t.Errorf("expected access denied message, got: %s", msg.Text)
	}
}

