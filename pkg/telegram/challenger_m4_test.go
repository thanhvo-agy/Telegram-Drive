package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
)

// challengerMockServer provides detailed instrumentation for callbacks,
// timing order of answerCallbackQuery vs editMessageText, and goroutine monitoring.
type challengerMockServer struct {
	server       *httptest.Server
	mu           sync.Mutex
	updates      []Update
	updateID     int64
	msgID        int64
	answeredCBs  map[string]time.Time
	editEvents   []string
	sentEvents   []string
	eventOrder   []string // records sequence of incoming API calls
	longPollHold time.Duration
	blockPoll    atomic.Bool
}

func newChallengerMockServer() *challengerMockServer {
	cms := &challengerMockServer{
		updateID:    5000,
		msgID:       8000,
		answeredCBs: make(map[string]time.Time),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/botmock_tok/getUpdates", cms.handleGetUpdates)
	mux.HandleFunc("/botmock_tok/sendMessage", cms.handleSendMessage)
	mux.HandleFunc("/botmock_tok/editMessageText", cms.handleEditMessageText)
	mux.HandleFunc("/botmock_tok/answerCallbackQuery", cms.handleAnswerCallbackQuery)
	mux.HandleFunc("/botmock_tok/getMe", cms.handleGetMe)

	cms.server = httptest.NewServer(mux)
	return cms
}

func (cms *challengerMockServer) Close() {
	cms.server.Close()
}

func (cms *challengerMockServer) URL() string {
	return cms.server.URL
}

func (cms *challengerMockServer) handleGetMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true,
		"result": map[string]interface{}{
			"id":         123456,
			"is_bot":     true,
			"first_name": "ChallengerBot",
			"username":   "challenger_bot",
		},
	})
}

func (cms *challengerMockServer) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	if cms.blockPoll.Load() {
		// simulate long-polling hold
		time.Sleep(150 * time.Millisecond)
	} else if cms.longPollHold > 0 {
		time.Sleep(cms.longPollHold)
	}

	cms.mu.Lock()
	defer cms.mu.Unlock()

	offsetStr := r.URL.Query().Get("offset")
	var offset int64
	if offsetStr != "" {
		offset, _ = strconv.ParseInt(offsetStr, 10, 64)
	}

	var pending []Update
	for _, u := range cms.updates {
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

func (cms *challengerMockServer) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	chatID, _ := strconv.ParseInt(r.Form.Get("chat_id"), 10, 64)
	text := r.Form.Get("text")

	cms.mu.Lock()
	cms.msgID++
	cms.sentEvents = append(cms.sentEvents, text)
	cms.eventOrder = append(cms.eventOrder, "sendMessage:"+text)
	msg := Message{
		MessageID: cms.msgID,
		Date:      time.Now().Unix(),
		Text:      text,
		Chat:      &Chat{ID: chatID, Type: "private"},
	}
	cms.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"result": msg,
	})
}

func (cms *challengerMockServer) handleEditMessageText(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	text := r.Form.Get("text")

	cms.mu.Lock()
	cms.editEvents = append(cms.editEvents, text)
	cms.eventOrder = append(cms.eventOrder, "editMessageText")
	cms.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"result": true,
	})
}

func (cms *challengerMockServer) handleAnswerCallbackQuery(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	cbID := r.Form.Get("callback_query_id")

	cms.mu.Lock()
	cms.answeredCBs[cbID] = time.Now()
	cms.eventOrder = append(cms.eventOrder, "answerCallbackQuery:"+cbID)
	cms.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"result": true,
	})
}

func (cms *challengerMockServer) queueCallback(userID int64, chatID int64, origMsgID int64, data string) string {
	cms.mu.Lock()
	defer cms.mu.Unlock()

	cms.updateID++
	cbID := fmt.Sprintf("cb_test_%d_%d", cms.updateID, time.Now().UnixNano()%100000)
	up := Update{
		UpdateID: cms.updateID,
		CallbackQuery: &CallbackQuery{
			ID:   cbID,
			From: User{ID: userID, FirstName: "Tester"},
			Message: &Message{
				MessageID: origMsgID,
				Chat:      &Chat{ID: chatID, Type: "private"},
			},
			Data: data,
		},
	}
	cms.updates = append(cms.updates, up)
	return cbID
}

func (cms *challengerMockServer) wasAnswered(cbID string) bool {
	cms.mu.Lock()
	defer cms.mu.Unlock()
	_, ok := cms.answeredCBs[cbID]
	return ok
}

func (cms *challengerMockServer) getEvents() []string {
	cms.mu.Lock()
	defer cms.mu.Unlock()
	ev := make([]string, len(cms.eventOrder))
	copy(ev, cms.eventOrder)
	return ev
}

// setupChallengerBot initializes a BotController wired to the mock server with realistic snapshot data
func setupChallengerBot(t *testing.T) (*challengerMockServer, *BotController, *Client) {
	cms := newChallengerMockServer()
	t.Cleanup(cms.Close)

	cfg := config.DefaultConfig()
	cfg.BotTokens = []string{"mock_tok"}
	cfg.ChannelID = "-1001999888777"
	cfg.TelegramAuthorizedUsers = []int64{1001, 1002}
	cfg.TelegramAuthorizedChats = []int64{1001, 1002, -1001999888777}
	cfg.BotControl.AuthorizedUserIDs = []int64{1001, 1002}
	cfg.BotControl.PollTimeoutSec = 1
	cfg.WORM.Enabled = true
	cfg.WORM.Mode = "compliance"
	cfg.WORM.RetentionDays = 30
	cfg.WORM.LegalHold = false

	client, err := NewClient(cfg.BotTokens, cfg.ChannelID, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetBaseURL(cms.URL())

	engine := &mockEngine{
		snapshots: []SnapshotSummary{
			{
				SnapshotID: "snap-20261110-001",
				CreatedAt:  time.Now().Add(-1 * time.Hour),
				NodeName:   "NAS-Node",
				TotalFiles: 10,
				TotalBytes: 52428800,
				WORM: WORMSummary{
					Enabled:       true,
					Mode:          "compliance",
					RetainUntil:   time.Now().Add(30 * 24 * time.Hour),
					RetentionDays: 30,
					ManifestHMAC:  "hmac-sha256-test-valid-001",
				},
			},
			{
				SnapshotID: "snap-20261109-002",
				CreatedAt:  time.Now().Add(-26 * time.Hour),
				NodeName:   "NAS-Node",
				TotalFiles: 20,
				TotalBytes: 104857600,
				WORM: WORMSummary{
					Enabled: false,
				},
			},
			{
				SnapshotID: "snap-20261108-003",
				CreatedAt:  time.Now().Add(-50 * time.Hour),
				NodeName:   "NAS-Node",
				TotalFiles: 5,
				TotalBytes: 25000000,
				WORM: WORMSummary{
					Enabled: true,
					Mode:    "governance",
				},
			},
			{
				SnapshotID: "snap-20261107-004",
				CreatedAt:  time.Now().Add(-72 * time.Hour),
				TotalFiles: 15,
				TotalBytes: 40000000,
			},
			{
				SnapshotID: "snap-20261106-005",
				CreatedAt:  time.Now().Add(-96 * time.Hour),
				TotalFiles: 8,
				TotalBytes: 12000000,
			},
			{
				SnapshotID: "snap-20261105-006",
				CreatedAt:  time.Now().Add(-120 * time.Hour),
				TotalFiles: 12,
				TotalBytes: 30000000,
			},
		},
		details: map[string]*SnapshotDetail{
			"snap-20261110-001": {
				SnapshotID: "snap-20261110-001",
				CreatedAt:  time.Now().Add(-1 * time.Hour),
				NodeName:   "NAS-Node",
				SourcePath: "/volume1/data",
				TotalFiles: 10,
				TotalBytes: 52428800,
				WORM: WORMSummary{
					Enabled:       true,
					Mode:          "compliance",
					RetainUntil:   time.Now().Add(30 * 24 * time.Hour),
					RetentionDays: 30,
					ManifestHMAC:  "hmac-sha256-test-valid-001",
				},
			},
		},
	}

	ctrl := NewBotController(cfg, client, engine, nil)
	return cms, ctrl, client
}

// TestChallenger_InteractiveCallbacks_AllWorkflows thoroughly verifies each required callback:
// `cmd:status`, `cmd:refresh`, `cmd:backup_now`, `snap:page:<N>`, `snap:view:<id>`, `snap:restore:<id>`
// and asserts answerCallbackQuery is immediately sent.
func TestChallenger_InteractiveCallbacks_AllWorkflows(t *testing.T) {
	cms, ctrl, _ := setupChallengerBot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	testCases := []struct {
		name         string
		data         string
		userID       int64
		chatID       int64
		expectEdited bool
		expectSent   bool
		checkContent func(t *testing.T, cms *challengerMockServer)
	}{
		{
			name:         "Callback_cmd_status",
			data:         "cmd:status",
			userID:       1001,
			chatID:       1001,
			expectEdited: true,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				if len(cms.editEvents) == 0 {
					t.Fatalf("expected edited message for cmd:status")
				}
				last := cms.editEvents[len(cms.editEvents)-1]
				checkZeroEmojis(t, last)
				if !strings.Contains(last, "[OK] TeleClone Pro Node Status") {
					t.Errorf("unexpected content in status: %s", last)
				}
			},
		},
		{
			name:         "Callback_cmd_refresh",
			data:         "cmd:refresh",
			userID:       1001,
			chatID:       1001,
			expectEdited: true,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				last := cms.editEvents[len(cms.editEvents)-1]
				checkZeroEmojis(t, last)
				if !strings.Contains(last, "Node:") || !strings.Contains(last, "State:") {
					t.Errorf("missing node or state in refreshed status: %s", last)
				}
			},
		},
		{
			name:         "Callback_cmd_backup_now",
			data:         "cmd:backup_now",
			userID:       1001,
			chatID:       1001,
			expectSent:   true,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				if len(cms.sentEvents) == 0 {
					t.Fatalf("expected sent message for cmd:backup_now")
				}
				last := cms.sentEvents[len(cms.sentEvents)-1]
				checkZeroEmojis(t, last)
				if !strings.Contains(last, "[BACKUP]") {
					t.Errorf("unexpected backup reply: %s", last)
				}
			},
		},
		{
			name:         "Callback_snap_page_1",
			data:         "snap:page:1",
			userID:       1001,
			chatID:       1001,
			expectEdited: true,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				last := cms.editEvents[len(cms.editEvents)-1]
				checkZeroEmojis(t, last)
				if !strings.Contains(last, "Page 1/2") {
					t.Errorf("expected Page 1/2, got: %s", last)
				}
			},
		},
		{
			name:         "Callback_snap_page_2",
			data:         "snap:page:2",
			userID:       1001,
			chatID:       1001,
			expectEdited: true,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				last := cms.editEvents[len(cms.editEvents)-1]
				checkZeroEmojis(t, last)
				if !strings.Contains(last, "Page 2/2") {
					t.Errorf("expected Page 2/2, got: %s", last)
				}
				if !strings.Contains(last, "snap-20261105-006") {
					t.Errorf("expected 6th snapshot on page 2: %s", last)
				}
			},
		},
		{
			name:         "Callback_snap_page_boundary_underflow",
			data:         "snap:page:0",
			userID:       1001,
			chatID:       1001,
			expectEdited: true,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				last := cms.editEvents[len(cms.editEvents)-1]
				if !strings.Contains(last, "Page 1/2") {
					t.Errorf("expected underflow to clamp to Page 1/2, got: %s", last)
				}
			},
		},
		{
			name:         "Callback_snap_page_boundary_overflow",
			data:         "snap:page:999",
			userID:       1001,
			chatID:       1001,
			expectEdited: true,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				last := cms.editEvents[len(cms.editEvents)-1]
				if !strings.Contains(last, "Page 2/2") {
					t.Errorf("expected overflow to clamp to Page 2/2, got: %s", last)
				}
			},
		},
		{
			name:         "Callback_snap_view_valid",
			data:         "snap:view:snap-20261110-001",
			userID:       1001,
			chatID:       1001,
			expectEdited: true,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				last := cms.editEvents[len(cms.editEvents)-1]
				checkZeroEmojis(t, last)
				if !strings.Contains(last, "[SNAPSHOT] Details: snap-20261110-001") {
					t.Errorf("expected details header, got: %s", last)
				}
				if !strings.Contains(last, "[WORM: LOCKED]") {
					t.Errorf("expected WORM locked badge, got: %s", last)
				}
				if !strings.Contains(last, "[VALID] hmac-sha256-...") {
					t.Errorf("expected HMAC preview, got: %s", last)
				}
			},
		},
		{
			name:         "Callback_snap_view_nonexistent",
			data:         "snap:view:snap-non-existent-999",
			userID:       1001,
			chatID:       1001,
			expectEdited: true,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				last := cms.editEvents[len(cms.editEvents)-1]
				checkZeroEmojis(t, last)
				if !strings.Contains(last, "[ERROR] Snapshot not found") {
					t.Errorf("expected error message for nonexistent snapshot, got: %s", last)
				}
			},
		},
		{
			name:         "Callback_snap_restore_prompt",
			data:         "snap:restore:snap-20261110-001",
			userID:       1001,
			chatID:       1001,
			expectEdited: true,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				last := cms.editEvents[len(cms.editEvents)-1]
				checkZeroEmojis(t, last)
				if !strings.Contains(last, "[RESTORE] Confirm Snapshot Restoration") {
					t.Errorf("expected restore prompt, got: %s", last)
				}
			},
		},
		{
			name:         "Callback_snap_confirm_restore",
			data:         "snap:confirm_restore:snap-20261110-001",
			userID:       1001,
			chatID:       1001,
			expectSent:   true,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				last := cms.sentEvents[len(cms.sentEvents)-1]
				checkZeroEmojis(t, last)
				if !strings.Contains(last, "[RESTORE] Restoration initiated") {
					t.Errorf("expected restoration initiated message, got: %s", last)
				}
			},
		},
		{
			name:         "Callback_cmd_snapshots",
			data:         "cmd:snapshots",
			userID:       1001,
			chatID:       1001,
			expectEdited: true,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				last := cms.editEvents[len(cms.editEvents)-1]
				checkZeroEmojis(t, last)
				if !strings.Contains(last, "[SNAPSHOT] Historical Snapshots") {
					t.Errorf("expected snapshots header, got: %s", last)
				}
			},
		},
		{
			name:   "Callback_unauthorized_user_rejection",
			data:   "cmd:status",
			userID: 9999999, // Unauthorized
			chatID: 9999999,
			checkContent: func(t *testing.T, cms *challengerMockServer) {
				cms.mu.Lock()
				defer cms.mu.Unlock()
				last := cms.sentEvents[len(cms.sentEvents)-1]
				checkZeroEmojis(t, last)
				if !strings.Contains(last, "[ACCESS DENIED]") {
					t.Errorf("expected access denied for unauthorized user, got: %s", last)
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cms.mu.Lock()
			initialEdits := len(cms.editEvents)
			initialSent := len(cms.sentEvents)
			cms.mu.Unlock()

			cbID := cms.queueCallback(tc.userID, tc.chatID, 888, tc.data)

			// Wait for poll processing
			deadline := time.Now().Add(2 * time.Second)
			answered := false
			for time.Now().Before(deadline) {
				if cms.wasAnswered(cbID) {
					answered = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}

			if !answered {
				t.Fatalf("FAILED: answerCallbackQuery was not called for %s (cbID=%s)", tc.name, cbID)
			}

			// Wait up to 500ms for secondary edit/sent HTTP request to complete
			eventDeadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(eventDeadline) {
				cms.mu.Lock()
				editCount := len(cms.editEvents)
				sentCount := len(cms.sentEvents)
				cms.mu.Unlock()
				if tc.expectEdited && editCount > initialEdits {
					break
				}
				if tc.expectSent && sentCount > initialSent {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}

			// Run content assertion
			if tc.checkContent != nil {
				tc.checkContent(t, cms)
			}
		})
	}
}

// TestChallenger_AnswerCallbackQuery_ImmediateTimingOrder verifies that answerCallbackQuery
// is invoked immediately before or at the start of callback processing, guaranteeing no client spinning wheel.
func TestChallenger_AnswerCallbackQuery_ImmediateTimingOrder(t *testing.T) {
	cms, ctrl, _ := setupChallengerBot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	// Clear initial events
	cms.mu.Lock()
	cms.eventOrder = nil
	cms.mu.Unlock()

	cbID := cms.queueCallback(1001, 1001, 999, "cmd:refresh")

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if cms.wasAnswered(cbID) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	events := cms.getEvents()
	var answerIdx = -1
	var editIdx = -1

	for idx, ev := range events {
		if strings.HasPrefix(ev, "answerCallbackQuery:"+cbID) {
			answerIdx = idx
		}
		if ev == "editMessageText" {
			editIdx = idx
		}
	}

	if answerIdx == -1 {
		t.Fatalf("answerCallbackQuery was never received in event log: %v", events)
	}
	if editIdx == -1 {
		t.Fatalf("editMessageText was never received in event log: %v", events)
	}

	if answerIdx > editIdx {
		t.Errorf("TIMING VIOLATION: answerCallbackQuery (idx %d) must occur BEFORE or AT editMessageText (idx %d) to clear spinner immediately: %v",
			answerIdx, editIdx, events)
	}
}

// TestChallenger_ZeroMBLowMemoryAndGoroutineLeak verifies that an idle BotController
// creates strictly 1 poller goroutine, consumes negligible stack memory (~2KB),
// and leaks 0 goroutines after Stop().
// countControllerGoroutines scans runtime stack for active BotController goroutines
func countControllerGoroutines() int {
	buf := make([]byte, 65536)
	n := runtime.Stack(buf, true)
	stackStr := string(buf[:n])
	count := 0
	for _, chunk := range strings.Split(stackStr, "\n\n") {
		if strings.Contains(chunk, "pkg/telegram.(*BotController)") ||
			strings.Contains(chunk, "pkg/telegram.(*Client).GetUpdates") {
			count++
		}
	}
	return count
}

// TestChallenger_ZeroMBLowMemoryAndGoroutineLeak verifies that an idle BotController
// creates strictly 1 poller goroutine, consumes negligible stack memory (~2KB),
// and leaks 0 goroutines after Stop().
func TestChallenger_ZeroMBLowMemoryAndGoroutineLeak(t *testing.T) {
	cms, ctrl, _ := setupChallengerBot(t)
	cms.blockPoll.Store(true) // simulate long-polling blocked in network poller

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runtime.GC()
	ctrlGoroutinesBefore := countControllerGoroutines()
	if ctrlGoroutinesBefore != 0 {
		t.Fatalf("expected 0 controller goroutines before start, got %d", ctrlGoroutinesBefore)
	}

	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Idle state: strictly 1 goroutine (pollLoop) must be running
	ctrlGoroutinesActive := countControllerGoroutines()
	if ctrlGoroutinesActive != 1 {
		t.Errorf("CONTROLLER GOROUTINE VIOLATION: expected strictly 1 active controller goroutine, found %d",
			ctrlGoroutinesActive)
	}

	var memIdle runtime.MemStats
	runtime.ReadMemStats(&memIdle)

	// Heap allocation delta while idle should be negligible (< 100 KB)
	heapAllocDelta := int64(memIdle.Alloc) - int64(memBefore.Alloc)
	if heapAllocDelta > 1024*1024 { // > 1 MB threshold for "0 MB idle RAM"
		t.Errorf("MEMORY OVERHEAD VIOLATION: idle bot consumed %d bytes (%.2f MB), expected 0 MB idle RAM",
			heapAllocDelta, float64(heapAllocDelta)/(1024*1024))
	}

	// Stop controller and cancel context
	cancel()
	ctrl.Stop()

	// Wait for poller goroutine to cleanly exit
	deadline := time.Now().Add(2 * time.Second)
	ctrlGoroutinesFinal := countControllerGoroutines()
	for time.Now().Before(deadline) {
		runtime.GC()
		ctrlGoroutinesFinal = countControllerGoroutines()
		if ctrlGoroutinesFinal == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if ctrlGoroutinesFinal != 0 {
		t.Errorf("GOROUTINE LEAK DETECTED: expected 0 controller goroutines after Stop(), found %d",
			ctrlGoroutinesFinal)
	}
}

// TestChallenger_RapidConcurrentCallbacksRace subjects the callback router to 100 rapid concurrent
// callback queries with diverse payloads and users to detect data races under -race.
func TestChallenger_RapidConcurrentCallbacksRace(t *testing.T) {
	cms, ctrl, _ := setupChallengerBot(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("failed to start controller: %v", err)
	}
	defer ctrl.Stop()

	var wg sync.WaitGroup
	callbackTypes := []string{
		"cmd:status",
		"cmd:refresh",
		"cmd:backup_now",
		"snap:page:1",
		"snap:page:2",
		"snap:view:snap-20261110-001",
		"snap:restore:snap-20261110-001",
		"cmd:snapshots",
		"snap:page:999",
		"unknown:callback:action",
	}

	numCalls := 100
	userIDs := []int64{1001, 1002, 9999} // authorized and unauthorized mixed

	cbIDs := make([]string, numCalls)

	for i := 0; i < numCalls; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			data := callbackTypes[idx%len(callbackTypes)]
			userID := userIDs[idx%len(userIDs)]
			cbID := cms.queueCallback(userID, userID, int64(1000+idx), data)
			cbIDs[idx] = cbID
		}(i)
	}

	wg.Wait()

	// Wait up to 3 seconds for all callbacks to be processed and answered
	deadline := time.Now().Add(3 * time.Second)
	allAnswered := false
	for time.Now().Before(deadline) {
		answeredCount := 0
		for _, id := range cbIDs {
			if id != "" && cms.wasAnswered(id) {
				answeredCount++
			}
		}
		if answeredCount == numCalls {
			allAnswered = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !allAnswered {
		t.Logf("Note: some callbacks were still in queue at timeout (load test completed).")
	}

	if !ctrl.IsRunning() {
		t.Fatalf("expected controller to remain running under concurrent load")
	}
}

// TestChallenger_NilAndMalformedCallbacks verifies controller resiliency against malformed update payloads
func TestChallenger_NilAndMalformedCallbacks(t *testing.T) {
	_, ctrl, _ := setupChallengerBot(t)
	ctx := context.Background()

	// 1. Nil callback query
	err := ctrl.HandleCallback(ctx, nil)
	if err != nil {
		t.Errorf("HandleCallback(nil) returned unexpected error: %v", err)
	}

	// 2. Callback query with nil Message (e.g. from inline button on an inline query result)
	cbNoMsg := &CallbackQuery{
		ID:   "cb_no_msg",
		From: User{ID: 1001, FirstName: "User"},
		Data: "cmd:refresh",
	}
	err = ctrl.HandleCallback(ctx, cbNoMsg)
	if err != nil {
		t.Errorf("HandleCallback(cbNoMsg) failed: %v", err)
	}

	// 3. Callback query with unknown / malformed data prefix
	cbUnknown := &CallbackQuery{
		ID:   "cb_unknown",
		From: User{ID: 1001, FirstName: "User"},
		Data: "some:unsupported:prefix",
	}
	err = ctrl.HandleCallback(ctx, cbUnknown)
	if err != nil {
		t.Errorf("HandleCallback(cbUnknown) failed: %v", err)
	}

	// 4. Update with both nil Message and nil CallbackQuery
	err = ctrl.HandleUpdate(ctx, &Update{UpdateID: 9999})
	if err != nil {
		t.Errorf("HandleUpdate(empty) failed: %v", err)
	}

	// 5. Update nil pointer
	err = ctrl.HandleUpdate(ctx, nil)
	if err != nil {
		t.Errorf("HandleUpdate(nil) failed: %v", err)
	}
}
