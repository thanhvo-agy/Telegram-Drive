package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MockTelegramUpdate represents an incoming update for long polling.
type MockTelegramUpdate struct {
	UpdateID      int64                `json:"update_id"`
	Message       *MockTelegramMessage `json:"message,omitempty"`
	CallbackQuery *MockCallbackQuery   `json:"callback_query,omitempty"`
}

type MockTelegramMessage struct {
	MessageID int64                 `json:"message_id"`
	From      MockTelegramUser      `json:"from"`
	Chat      MockTelegramChat      `json:"chat"`
	Date      int64                 `json:"date"`
	Text      string                `json:"text,omitempty"`
	Document  *MockTelegramDocument `json:"document,omitempty"`
	Caption   string                `json:"caption,omitempty"`
}

type MockTelegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

type MockTelegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type MockTelegramDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

type MockCallbackQuery struct {
	ID      string               `json:"id"`
	From    MockTelegramUser     `json:"from"`
	Message *MockTelegramMessage `json:"message,omitempty"`
	Data    string               `json:"data,omitempty"`
}

type MockSentMessage struct {
	ChatID      string `json:"chat_id"`
	Text        string `json:"text"`
	ParseMode   string `json:"parse_mode"`
	ReplyMarkup string `json:"reply_markup"`
	Timestamp   time.Time
}

type MockEditedMessage struct {
	ChatID      string `json:"chat_id"`
	MessageID   int64  `json:"message_id"`
	Text        string `json:"text"`
	ReplyMarkup string `json:"reply_markup"`
}

type MockAnsweredCallback struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text"`
	ShowAlert       bool   `json:"show_alert"`
}

// MockTelegramServer provides an in-memory simulation of api.telegram.org.
type MockTelegramServer struct {
	server       *httptest.Server
	mu           sync.Mutex
	updateQueue  []MockTelegramUpdate
	updateCond   *sync.Cond
	nextUpdateID int64
	nextMsgID    int64
	nextFileID   int64

	SentMessages      []MockSentMessage
	EditedMessages    []MockEditedMessage
	AnsweredCallbacks []MockAnsweredCallback
	UploadedFiles     map[string][]byte
	FileCaptions      map[string]string

	rateLimitUntil time.Time
	rateLimitSec   int
	dropNextConn   atomic.Bool
	lastOffset     int64
	getUpdatesWait bool
}

// NewMockTelegramServer creates and starts a new MockTelegramServer.
func NewMockTelegramServer() *MockTelegramServer {
	s := &MockTelegramServer{
		updateQueue:   make([]MockTelegramUpdate, 0),
		UploadedFiles: make(map[string][]byte),
		FileCaptions:  make(map[string]string),
		nextUpdateID:  1000,
		nextMsgID:     5000,
		nextFileID:    1,
	}
	s.updateCond = sync.NewCond(&s.mu)

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleTelegramRequest)
	s.server = httptest.NewServer(mux)
	return s
}

// URL returns the mock server's base URL.
func (s *MockTelegramServer) URL() string {
	return s.server.URL
}

// Close terminates the mock server.
func (s *MockTelegramServer) Close() {
	s.server.Close()
}

// InjectMessage enqueues an incoming text message from a user.
func (s *MockTelegramServer) InjectMessage(userID int64, username string, chatID int64, text string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	upID := s.nextUpdateID
	s.nextUpdateID++
	msgID := s.nextMsgID
	s.nextMsgID++

	up := MockTelegramUpdate{
		UpdateID: upID,
		Message: &MockTelegramMessage{
			MessageID: msgID,
			From: MockTelegramUser{
				ID:        userID,
				IsBot:     false,
				FirstName: "TestUser",
				Username:  username,
			},
			Chat: MockTelegramChat{
				ID:   chatID,
				Type: "private",
			},
			Date: time.Now().Unix(),
			Text: text,
		},
	}
	s.updateQueue = append(s.updateQueue, up)
	s.updateCond.Broadcast()
	return msgID
}

// InjectCallback enqueues an inline button callback query.
func (s *MockTelegramServer) InjectCallback(userID int64, username string, chatID int64, origMsgID int64, data string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	upID := s.nextUpdateID
	s.nextUpdateID++
	cbID := fmt.Sprintf("cb_%d", time.Now().UnixNano())

	up := MockTelegramUpdate{
		UpdateID: upID,
		CallbackQuery: &MockCallbackQuery{
			ID: cbID,
			From: MockTelegramUser{
				ID:        userID,
				IsBot:     false,
				FirstName: "TestUser",
				Username:  username,
			},
			Message: &MockTelegramMessage{
				MessageID: origMsgID,
				Chat: MockTelegramChat{
					ID:   chatID,
					Type: "private",
				},
				Date: time.Now().Unix(),
			},
			Data: data,
		},
	}
	s.updateQueue = append(s.updateQueue, up)
	s.updateCond.Broadcast()
	return cbID
}

// SetRateLimit configures the server to return HTTP 429 for the specified duration.
func (s *MockTelegramServer) SetRateLimit(retryAfterSec int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimitSec = retryAfterSec
	s.rateLimitUntil = time.Now().Add(time.Duration(retryAfterSec) * time.Second)
}

// TriggerNetworkDrop causes the next request to be aborted immediately.
func (s *MockTelegramServer) TriggerNetworkDrop() {
	s.dropNextConn.Store(true)
}

// GetLastSentMessage returns the most recent message posted by tgdrive.
func (s *MockTelegramServer) GetLastSentMessage() (MockSentMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.SentMessages) == 0 {
		return MockSentMessage{}, false
	}
	return s.SentMessages[len(s.SentMessages)-1], true
}

// GetSentMessagesCount returns total messages sent to mock telegram.
func (s *MockTelegramServer) GetSentMessagesCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.SentMessages)
}

func (s *MockTelegramServer) handleTelegramRequest(w http.ResponseWriter, r *http.Request) {
	if s.dropNextConn.CompareAndSwap(true, false) {
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

	s.mu.Lock()
	if time.Now().Before(s.rateLimitUntil) {
		retrySec := s.rateLimitSec
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		resp := map[string]interface{}{
			"ok":          false,
			"error_code":  429,
			"description": "Too Many Requests: retry after " + strconv.Itoa(retrySec),
			"parameters": map[string]interface{}{
				"retry_after": retrySec,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	s.mu.Unlock()

	path := r.URL.Path

	// Handle file download: /file/bot<token>/...
	if strings.HasPrefix(path, "/file/bot") {
		parts := strings.Split(path, "/")
		if len(parts) >= 4 {
			fileID := parts[len(parts)-1]
			s.mu.Lock()
			data, found := s.UploadedFiles[fileID]
			s.mu.Unlock()
			if found {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(data)
				return
			}
		}
		http.NotFound(w, r)
		return
	}

	// Dispatch Bot API calls: /bot<token>/<method>
	if strings.Contains(path, "/getMe") {
		s.handleGetMe(w, r)
		return
	}
	if strings.Contains(path, "/getUpdates") {
		s.handleGetUpdates(w, r)
		return
	}
	if strings.Contains(path, "/sendMessage") {
		s.handleSendMessage(w, r)
		return
	}
	if strings.Contains(path, "/sendDocument") {
		s.handleSendDocument(w, r)
		return
	}
	if strings.Contains(path, "/getFile") {
		s.handleGetFile(w, r)
		return
	}
	if strings.Contains(path, "/answerCallbackQuery") {
		s.handleAnswerCallback(w, r)
		return
	}
	if strings.Contains(path, "/editMessageText") {
		s.handleEditMessageText(w, r)
		return
	}

	// Default fallback: 200 OK
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": true})
}

func (s *MockTelegramServer) handleGetMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"ok": true,
		"result": map[string]interface{}{
			"id":         987654321,
			"is_bot":     true,
			"first_name": "TeleCloneBot",
			"username":   "teleclone_test_bot",
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *MockTelegramServer) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	offsetStr := r.URL.Query().Get("offset")
	var offset int64
	if offsetStr != "" {
		offset, _ = strconv.ParseInt(offsetStr, 10, 64)
	}

	timeoutSec := 0
	timeoutStr := r.URL.Query().Get("timeout")
	if timeoutStr != "" {
		timeoutSec, _ = strconv.Atoi(timeoutStr)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastOffset = offset

	// Filter queue: only keep updates with ID >= offset
	filtered := make([]MockTelegramUpdate, 0)
	for _, up := range s.updateQueue {
		if up.UpdateID >= offset {
			filtered = append(filtered, up)
		}
	}

	// If no updates and timeout requested, short wait or return empty
	if len(filtered) == 0 && timeoutSec > 0 {
		// Non-blocking in tests or wait max 50ms to keep tests fast
		waitLimit := time.After(50 * time.Millisecond)
		waitDone := make(chan struct{})
		go func() {
			s.mu.Lock()
			for len(s.updateQueue) == 0 {
				s.updateCond.Wait()
			}
			s.mu.Unlock()
			close(waitDone)
		}()
		select {
		case <-waitDone:
			for _, up := range s.updateQueue {
				if up.UpdateID >= offset {
					filtered = append(filtered, up)
				}
			}
		case <-waitLimit:
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"result": filtered,
	})
}

func (s *MockTelegramServer) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	chatID := r.Form.Get("chat_id")
	text := r.Form.Get("text")
	parseMode := r.Form.Get("parse_mode")
	replyMarkup := r.Form.Get("reply_markup")

	s.mu.Lock()
	msg := MockSentMessage{
		ChatID:      chatID,
		Text:        text,
		ParseMode:   parseMode,
		ReplyMarkup: replyMarkup,
		Timestamp:   time.Now(),
	}
	s.SentMessages = append(s.SentMessages, msg)
	msgID := s.nextMsgID
	s.nextMsgID++
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true,
		"result": map[string]interface{}{
			"message_id": msgID,
			"date":       time.Now().Unix(),
			"text":       text,
		},
	})
}

func (s *MockTelegramServer) handleSendDocument(w http.ResponseWriter, r *http.Request) {
	err := r.ParseMultipartForm(64 * 1024 * 1024)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	caption := r.FormValue("caption")

	file, header, err := r.FormFile("document")
	var fileData []byte
	var fileName string
	if err == nil && file != nil {
		defer file.Close()
		fileName = header.Filename
		fileData, _ = io.ReadAll(file)
	}

	s.mu.Lock()
	fID := fmt.Sprintf("file_%d", s.nextFileID)
	s.nextFileID++
	msgID := s.nextMsgID
	s.nextMsgID++

	s.UploadedFiles[fID] = fileData
	s.FileCaptions[fID] = caption
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true,
		"result": map[string]interface{}{
			"message_id": msgID,
			"date":       time.Now().Unix(),
			"caption":    caption,
			"document": map[string]interface{}{
				"file_id":   fID,
				"file_name": fileName,
				"file_size": len(fileData),
			},
		},
	})
}

func (s *MockTelegramServer) handleGetFile(w http.ResponseWriter, r *http.Request) {
	fileID := r.URL.Query().Get("file_id")

	s.mu.Lock()
	data, found := s.UploadedFiles[fileID]
	s.mu.Unlock()

	if !found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":          false,
			"error_code":  404,
			"description": "File not found",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true,
		"result": map[string]interface{}{
			"file_id":   fileID,
			"file_path": "documents/" + fileID,
			"file_size": len(data),
		},
	})
}

func (s *MockTelegramServer) handleAnswerCallback(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	cbID := r.Form.Get("callback_query_id")
	text := r.Form.Get("text")
	showAlert := r.Form.Get("show_alert") == "true"

	s.mu.Lock()
	s.AnsweredCallbacks = append(s.AnsweredCallbacks, MockAnsweredCallback{
		CallbackQueryID: cbID,
		Text:            text,
		ShowAlert:       showAlert,
	})
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": true})
}

func (s *MockTelegramServer) handleEditMessageText(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	chatID := r.Form.Get("chat_id")
	msgID, _ := strconv.ParseInt(r.Form.Get("message_id"), 10, 64)
	text := r.Form.Get("text")
	markup := r.Form.Get("reply_markup")

	s.mu.Lock()
	s.EditedMessages = append(s.EditedMessages, MockEditedMessage{
		ChatID:      chatID,
		MessageID:   msgID,
		Text:        text,
		ReplyMarkup: markup,
	})
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true,
		"result": map[string]interface{}{
			"message_id": msgID,
			"text":       text,
		},
	})
}
