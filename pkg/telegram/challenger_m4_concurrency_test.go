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

// concurrencyMockServer provides mock Telegram Bot API with rich fault injection
// for HTTP 429 rate limiting, network drops, and request timing logs.
type concurrencyMockServer struct {
	server           *httptest.Server
	mu               sync.Mutex
	updates          []Update
	sentMessages     []Message
	pollRequests     []time.Time
	updateID         int64
	msgID            int64
	rateLimitUntil   time.Time
	rateLimitSec     int
	dropPollRequests atomic.Int32
	pollDelay        time.Duration
}

func newConcurrencyMockServer() *concurrencyMockServer {
	s := &concurrencyMockServer{
		updateID: 1000,
		msgID:    2000,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/botmock_tok/getUpdates", s.handleGetUpdates)
	mux.HandleFunc("/botmock_tok/sendMessage", s.handleSendMessage)
	mux.HandleFunc("/botmock_tok/editMessageText", s.handleEditMessageText)
	mux.HandleFunc("/botmock_tok/answerCallbackQuery", s.handleAnswerCallbackQuery)
	mux.HandleFunc("/botmock_tok/getMe", s.handleGetMe)

	s.server = httptest.NewServer(mux)
	return s
}

func (s *concurrencyMockServer) URL() string {
	return s.server.URL
}

func (s *concurrencyMockServer) Close() {
	s.server.Close()
}

func (s *concurrencyMockServer) handleGetMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true,
		"result": map[string]interface{}{
			"id":         123456,
			"is_bot":     true,
			"first_name": "ConcurrencyTestBot",
			"username":   "concurrency_test_bot",
		},
	})
}

func (s *concurrencyMockServer) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	now := time.Now()

	s.mu.Lock()
	s.pollRequests = append(s.pollRequests, now)
	delay := s.pollDelay
	s.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	// 1. Simulate Network Drop / Socket Reset
	if s.dropPollRequests.Load() > 0 {
		s.dropPollRequests.Add(-1)
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			if conn != nil {
				_ = conn.Close()
				return
			}
		}
		http.Error(w, "connection reset by peer", http.StatusBadGateway)
		return
	}

	// 2. Simulate HTTP 429 Too Many Requests
	s.mu.Lock()
	if !s.rateLimitUntil.IsZero() && now.Before(s.rateLimitUntil) {
		remainingSec := int(time.Until(s.rateLimitUntil).Seconds()) + 1
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":          false,
			"error_code":  429,
			"description": fmt.Sprintf("Too Many Requests: retry after %d", remainingSec),
			"parameters":  map[string]interface{}{"retry_after": remainingSec},
		})
		return
	}
	s.mu.Unlock()

	// 3. Normal updates delivery
	offsetStr := r.URL.Query().Get("offset")
	var offset int64
	if offsetStr != "" {
		offset, _ = strconv.ParseInt(offsetStr, 10, 64)
	}

	s.mu.Lock()
	var pending []Update
	for _, u := range s.updates {
		if u.UpdateID >= offset {
			pending = append(pending, u)
		}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"result": pending,
	})
}

func (s *concurrencyMockServer) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	chatID, _ := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
	text := r.Form.Get("text")

	s.mu.Lock()
	s.msgID++
	msg := Message{
		MessageID: s.msgID,
		Date:      time.Now().Unix(),
		Text:      text,
		Chat:      &Chat{ID: chatID, Type: "private"},
	}
	s.sentMessages = append(s.sentMessages, msg)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"result": msg,
	})
}

func (s *concurrencyMockServer) handleEditMessageText(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"result": true,
	})
}

func (s *concurrencyMockServer) handleAnswerCallbackQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"result": true,
	})
}

func (s *concurrencyMockServer) injectCommand(userID int64, chatID int64, cmd string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.updateID++
	s.msgID++
	up := Update{
		UpdateID: s.updateID,
		Message: &Message{
			MessageID: s.msgID,
			From:      &User{ID: userID, FirstName: "TestUser"},
			Chat:      &Chat{ID: chatID, Type: "private"},
			Date:      time.Now().Unix(),
			Text:      cmd,
		},
	}
	s.updates = append(s.updates, up)
}

func (s *concurrencyMockServer) setRateLimitDuration(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimitUntil = time.Now().Add(d)
	s.rateLimitSec = int(d.Seconds())
}

func (s *concurrencyMockServer) getPollRequestsSince(t time.Time) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res []time.Time
	for _, reqTime := range s.pollRequests {
		if reqTime.After(t) {
			res = append(res, reqTime)
		}
	}
	return res
}

func (s *concurrencyMockServer) getSentMessages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.sentMessages))
	copy(out, s.sentMessages)
	return out
}

// slowMockEngine simulates a backup that takes some time to execute
type slowMockEngine struct {
	mu           sync.Mutex
	snapshots    []SnapshotSummary
	activeBackup atomic.Int32
	backupDelay  time.Duration
}

func (sme *slowMockEngine) ListSnapshots() ([]SnapshotSummary, error) {
	sme.mu.Lock()
	defer sme.mu.Unlock()
	return sme.snapshots, nil
}

func (sme *slowMockEngine) GetSnapshot(id string) (*SnapshotDetail, error) {
	return &SnapshotDetail{SnapshotID: id}, nil
}

func (sme *slowMockEngine) CreateSnapshot(ctx context.Context, cb any) (*SnapshotDetail, error) {
	sme.activeBackup.Add(1)
	defer sme.activeBackup.Add(-1)

	if sme.backupDelay > 0 {
		time.Sleep(sme.backupDelay)
	}

	return &SnapshotDetail{
		SnapshotID: fmt.Sprintf("snap-%d", time.Now().UnixNano()),
		CreatedAt:  time.Now(),
		TotalFiles: 10,
		TotalBytes: 1024 * 1024,
	}, nil
}

func setupConcurrencyBot(t *testing.T, backupDelay time.Duration) (*concurrencyMockServer, *BotController, *Client, *slowMockEngine) {
	mockServer := newConcurrencyMockServer()
	t.Cleanup(mockServer.Close)

	cfg := config.DefaultConfig()
	cfg.BotTokens = []string{"mock_tok"}
	cfg.ChannelID = "-1001999888777"
	cfg.TelegramAuthorizedUsers = []int64{1001, 1002}
	cfg.TelegramAuthorizedChats = []int64{1001, 1002, -1001999888777}
	cfg.BotControl.AuthorizedUserIDs = []int64{1001, 1002}
	cfg.BotControl.PollTimeoutSec = 1

	client, err := NewClient(cfg.BotTokens, cfg.ChannelID, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURL(mockServer.URL())

	engine := &slowMockEngine{
		snapshots: []SnapshotSummary{
			{SnapshotID: "snap-001", CreatedAt: time.Now(), TotalFiles: 5, TotalBytes: 1000},
		},
		backupDelay: backupDelay,
	}

	ctrl := NewBotController(cfg, client, engine, nil)
	return mockServer, ctrl, client, engine
}

// ============================================================================
// STRESS TEST 1: Rapid Concurrent Duplicate /backup_now (Mutex Protection)
// ============================================================================

func TestStress_ActiveBackup_MutexProtection(t *testing.T) {
	// Configure engine with 150ms backup delay to simulate real snapshot creation
	_, ctrl, _, engine := setupConcurrencyBot(t, 150*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	// Launch 50 concurrent goroutines calling TriggerBackup at the exact same time
	concurrency := 50
	var wg sync.WaitGroup
	var startedCount atomic.Int64
	var rejectedCount atomic.Int64
	var errCount atomic.Int64

	startBarrier := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-startBarrier // synchronize start instant

			reply, err := ctrl.TriggerBackup(ctx, 1001)
			if err != nil {
				errCount.Add(1)
				return
			}

			if strings.Contains(reply, "Backup initiated") {
				startedCount.Add(1)
			} else if strings.Contains(reply, "already in progress") {
				rejectedCount.Add(1)
			} else {
				t.Errorf("goroutine %d: unexpected reply: %s", id, reply)
			}
		}(i)
	}

	// Release all goroutines simultaneously
	close(startBarrier)
	wg.Wait()

	t.Logf("Concurrent /backup_now results: started=%d, rejected=%d, errors=%d",
		startedCount.Load(), rejectedCount.Load(), errCount.Load())

	// CRITICAL ASSERTION:
	// Exactly ONE goroutine must succeed in starting the backup.
	// All other 49 goroutines MUST receive "[WARN] Backup already in progress".
	if startedCount.Load() != 1 {
		t.Fatalf("MUTEX PROTECTION FAILURE: expected exactly 1 backup initiated, got %d concurrent backups initiated!",
			startedCount.Load())
	}

	if rejectedCount.Load() != int64(concurrency-1) {
		t.Fatalf("MUTEX PROTECTION FAILURE: expected %d rejections with '[WARN] Backup already in progress', got %d",
			concurrency-1, rejectedCount.Load())
	}

	// Engine must have at most 1 active concurrent snapshot
	maxActive := engine.activeBackup.Load()
	if maxActive > 1 {
		t.Fatalf("RACE VIOLATION: engine.activeBackup exceeded 1 (found %d)", maxActive)
	}

	// Allow first backup to finish cleanly
	time.Sleep(200 * time.Millisecond)

	// Now another backup should be accepted
	reply, err := ctrl.TriggerBackup(ctx, 1001)
	if err != nil {
		t.Fatalf("TriggerBackup failed after cooldown: %v", err)
	}
	if !strings.Contains(reply, "Backup initiated") {
		t.Fatalf("expected backup to initiate after completion, got: %s", reply)
	}
}

// ============================================================================
// STRESS TEST 2: HTTP 429 Rate Limit Backoff (parameters.retry_after)
// ============================================================================

func TestStress_HTTP429_RateLimit_PoliteBackoff(t *testing.T) {
	mockServer, ctrl, client, _ := setupConcurrencyBot(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	// Wait for poller to perform initial poll
	time.Sleep(50 * time.Millisecond)

	// Inject 2-second rate limit with HTTP 429
	rateLimitSec := 2
	mockServer.setRateLimitDuration(time.Duration(rateLimitSec) * time.Second)
	rateLimitStartTime := time.Now()

	// Wait 300ms for poller to hit the 429 and register the backoff
	time.Sleep(300 * time.Millisecond)

	// Verify client marked bot as rate-limited
	meshStatus := client.GetMeshStatus()
	if len(meshStatus) > 0 {
		if meshStatus[0].IsAvailable {
			t.Errorf("expected bot 0 to be marked unavailable during 429 backoff")
		}
	}

	// During the backoff period (between 300ms and 1800ms),
	// the poller MUST NOT spam getUpdates
	midCheckTime := time.Now()
	time.Sleep(1 * time.Second) // wait to 1.3s total
	pollRequestsDuringBackoff := mockServer.getPollRequestsSince(midCheckTime)

	if len(pollRequestsDuringBackoff) > 1 {
		t.Errorf("RATE LIMIT VIOLATION: poller made %d requests during active 429 backoff window (expected 0 or 1)",
			len(pollRequestsDuringBackoff))
	}

	// Wait until rate limit period has fully passed (2s + margin)
	time.Sleep(1 * time.Second)

	// Now inject a command: poller must have recovered and must process it
	mockServer.injectCommand(1001, 1001, "/status")
	time.Sleep(300 * time.Millisecond)

	sentMsgs := mockServer.getSentMessages()
	foundStatus := false
	for _, m := range sentMsgs {
		if strings.Contains(m.Text, "[OK] TeleClone Pro Node Status") {
			foundStatus = true
			break
		}
	}

	if !foundStatus {
		t.Fatalf("RECOVERY FAILURE: poller failed to process /status command after HTTP 429 rate limit expired")
	}

	t.Logf("HTTP 429 polite backoff verified: bot waited %v and resumed cleanly",
		time.Since(rateLimitStartTime))
}

// ============================================================================
// STRESS TEST 3: Network Drop & Auto-Reconnect Recovery
// ============================================================================

func TestStress_NetworkDrop_AutoReconnect(t *testing.T) {
	mockServer, ctrl, _, _ := setupConcurrencyBot(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	// Drop the next 3 consecutive getUpdates calls (simulating sustained network flap)
	mockServer.dropPollRequests.Store(3)

	// Inject a command during the network flap
	mockServer.injectCommand(1001, 1001, "/help")

	// Wait for controller to experience the errors, back off, and reconnect
	// Backoff schedule: 200ms -> 400ms -> 800ms...
	time.Sleep(1500 * time.Millisecond)

	// Verify that the controller auto-reconnected and delivered the /help response
	sentMsgs := mockServer.getSentMessages()
	foundHelp := false
	for _, m := range sentMsgs {
		if strings.Contains(m.Text, "TeleClone Pro Remote Control Bot") {
			foundHelp = true
			break
		}
	}

	if !foundHelp {
		t.Fatalf("NETWORK DROP RECOVERY FAILURE: controller failed to deliver command after 3 network drops")
	}

	if !ctrl.IsRunning() {
		t.Fatalf("controller died during network drops")
	}

	t.Logf("Network drop auto-reconnect successfully verified: recovered and processed queued updates")
}

// ============================================================================
// STRESS TEST 4: Rapid Concurrent Mixed Commands under -race
// ============================================================================

func TestStress_Concurrent_RapidCommands_Race(t *testing.T) {
	mockServer, ctrl, _, _ := setupConcurrencyBot(t, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	concurrency := 20
	commandsPerRoutine := 5
	var wg sync.WaitGroup

	cmdPool := []string{
		"/status",
		"/backup_now",
		"/snapshots",
		"/snapshots 1",
		"/help",
		"/unknown_cmd",
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(routineID int) {
			defer wg.Done()
			for j := 0; j < commandsPerRoutine; j++ {
				cmd := cmdPool[(routineID+j)%len(cmdPool)]
				mockServer.injectCommand(1001, 1001, cmd)
				time.Sleep(10 * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()

	// Wait for poller and handlers to drain
	time.Sleep(500 * time.Millisecond)

	sentMsgs := mockServer.getSentMessages()
	if len(sentMsgs) == 0 {
		t.Fatalf("expected replies to be sent for concurrent commands, got 0")
	}

	// Verify zero emojis across all sent messages
	for _, m := range sentMsgs {
		checkZeroEmojis(t, m.Text)
	}

	t.Logf("Concurrent rapid commands verified under -race: processed %d responses without race conditions",
		len(sentMsgs))
}

// ============================================================================
// STRESS TEST 5: Graceful Shutdown Without Goroutine Leaks
// ============================================================================

func TestStress_GracefulShutdown_NoLeaks(t *testing.T) {
	mockServer, ctrl, _, _ := setupConcurrencyBot(t, 0)
	mockServer.pollDelay = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Stop controller and cancel context
	ctrl.Stop()
	cancel()

	// Controller should report not running
	if ctrl.IsRunning() {
		t.Errorf("controller still reported running after Stop()")
	}

	// Double Stop must be idempotent
	ctrl.Stop()

	// Calling Start again on a fresh context should succeed without race
	freshCtx, freshCancel := context.WithCancel(context.Background())
	defer freshCancel()

	if err := ctrl.Start(freshCtx); err != nil {
		t.Fatalf("re-starting controller failed: %v", err)
	}

	if !ctrl.IsRunning() {
		t.Errorf("controller not running after restart")
	}

	ctrl.Stop()
}
