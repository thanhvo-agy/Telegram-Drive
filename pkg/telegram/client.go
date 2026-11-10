package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/qos"
)

func apiBaseURL() string {
	if u := os.Getenv("TELEGRAM_API_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://api.telegram.org"
}

// BotStatus tracks live state and statistics of a bot in the mesh
type BotStatus struct {
	TokenPrefix  string    `json:"token_prefix"`
	Username     string    `json:"username"`
	IsAvailable  bool      `json:"is_available"`
	BlockedUntil time.Time `json:"blocked_until"`
	RequestsSent int64     `json:"requests_sent"`
	BytesSent    int64     `json:"bytes_sent"`
	LastError    string    `json:"last_error,omitempty"`
}

// Client represents the Multi-Bot Telegram client
type Client struct {
	tokens       []string
	channelID    string
	httpClient   *http.Client
	throttler    *qos.BandwidthThrottler
	botIndex     uint64
	baseURL      string
	mu           sync.RWMutex
	blockedBots  map[int]time.Time
	botStats     map[int]*BotStatus
}

// Telegram API response structures
type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Parameters  *apiParameters  `json:"parameters,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

type apiParameters struct {
	RetryAfter int `json:"retry_after,omitempty"`
}

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type Message struct {
	MessageID   int64                 `json:"message_id"`
	From        *User                 `json:"from,omitempty"`
	Chat        *Chat                 `json:"chat,omitempty"`
	Date        int64                 `json:"date"`
	Text        string                `json:"text,omitempty"`
	Document    *Document             `json:"document,omitempty"`
	Caption     string                `json:"caption,omitempty"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

type TGFile struct {
	FileID   string `json:"file_id"`
	FilePath string `json:"file_path"`
	FileSize int64  `json:"file_size"`
}

// NewClient initializes the Multi-Bot Telegram mesh
func NewClient(tokens []string, channelID string, throttler *qos.BandwidthThrottler) (*Client, error) {
	if len(tokens) == 0 {
		return nil, errors.New("no telegram bot tokens provided")
	}
	if channelID == "" {
		return nil, errors.New("channel_id is required")
	}

	c := &Client{
		tokens:      tokens,
		channelID:   channelID,
		throttler:   throttler,
		httpClient:  &http.Client{Timeout: 180 * time.Second},
		blockedBots: make(map[int]time.Time),
		botStats:    make(map[int]*BotStatus),
	}

	for i, tok := range tokens {
		prefix := tok
		if len(tok) > 10 {
			prefix = tok[:8] + "..."
		}
		c.botStats[i] = &BotStatus{
			TokenPrefix: prefix,
			IsAvailable: true,
		}
	}

	return c, nil
}

// pickBot selects the next available bot token in the mesh (Round-Robin with 429 backoff)
func (c *Client) pickBot() (int, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	total := len(c.tokens)

	for attempt := 0; attempt < total; attempt++ {
		idx := int(atomic.AddUint64(&c.botIndex, 1) % uint64(total))
		blockedUntil, isBlocked := c.blockedBots[idx]
		if isBlocked {
			if now.After(blockedUntil) {
				delete(c.blockedBots, idx)
				c.botStats[idx].IsAvailable = true
				return idx, c.tokens[idx], nil
			}
			continue
		}
		return idx, c.tokens[idx], nil
	}

	// If all bots are temporarily rate limited, find the one with the shortest wait
	var shortestWait time.Duration = 60 * time.Second
	bestIdx := 0
	for idx, blockedUntil := range c.blockedBots {
		wait := blockedUntil.Sub(now)
		if wait < shortestWait {
			shortestWait = wait
			bestIdx = idx
		}
	}

	if shortestWait > 0 {
		time.Sleep(shortestWait)
		delete(c.blockedBots, bestIdx)
		c.botStats[bestIdx].IsAvailable = true
	}

	return bestIdx, c.tokens[bestIdx], nil
}

func (c *Client) markRateLimited(botIdx int, retryAfterSeconds int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if retryAfterSeconds <= 0 {
		retryAfterSeconds = 10
	}
	until := time.Now().Add(time.Duration(retryAfterSeconds) * time.Second)
	c.blockedBots[botIdx] = until
	c.botStats[botIdx].IsAvailable = false
	c.botStats[botIdx].BlockedUntil = until
	c.botStats[botIdx].LastError = fmt.Sprintf("429 rate limit (retry after %ds)", retryAfterSeconds)
}

// UploadDocument uploads a chunk to Telegram channel with QoS throttling and automatic bot failover
func (c *Client) UploadDocument(ctx context.Context, filename string, data []byte, caption string) (*Document, int64, error) {
	maxRetries := len(c.tokens) * 2
	if maxRetries < 3 {
		maxRetries = 3
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		botIdx, token, err := c.pickBot()
		if err != nil {
			return nil, 0, err
		}

		doc, msgID, err := c.doUpload(ctx, botIdx, token, filename, data, caption)
		if err == nil {
			atomic.AddInt64(&c.botStats[botIdx].RequestsSent, 1)
			atomic.AddInt64(&c.botStats[botIdx].BytesSent, int64(len(data)))
			return doc, msgID, nil
		}

		lastErr = err
		// If context cancelled, abort
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, 0, ctx.Err()
		}

		// Wait slightly before trying next bot
		time.Sleep(500 * time.Millisecond)
	}

	return nil, 0, fmt.Errorf("upload failed after %d attempts: %w", maxRetries, lastErr)
}

func (c *Client) doUpload(ctx context.Context, botIdx int, token, filename string, data []byte, caption string) (*Document, int64, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// chat_id
	if err := writer.WriteField("chat_id", c.channelID); err != nil {
		return nil, 0, err
	}

	// caption
	if caption != "" {
		if err := writer.WriteField("caption", caption); err != nil {
			return nil, 0, err
		}
	}

	// document part
	part, err := writer.CreateFormFile("document", filename)
	if err != nil {
		return nil, 0, err
	}

	// Apply QoS throttling if configured
	var reader io.Reader = bytes.NewReader(data)
	if c.throttler != nil {
		reader = qos.NewThrottledReader(reader, c.throttler)
	}

	if _, err := io.Copy(part, reader); err != nil {
		return nil, 0, err
	}

	if err := writer.Close(); err != nil {
		return nil, 0, err
	}

	reqURL := fmt.Sprintf("%s/bot%s/sendDocument", c.apiBaseURL(), token)
	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}

	var tgResp apiResponse
	if err := json.Unmarshal(respBytes, &tgResp); err != nil {
		return nil, 0, fmt.Errorf("unmarshal telegram response: %w", err)
	}

	if !tgResp.OK {
		if tgResp.ErrorCode == 429 && tgResp.Parameters != nil {
			c.markRateLimited(botIdx, tgResp.Parameters.RetryAfter)
			return nil, 0, fmt.Errorf("rate limited (429): retry after %d", tgResp.Parameters.RetryAfter)
		}
		c.mu.Lock()
		c.botStats[botIdx].LastError = tgResp.Description
		c.mu.Unlock()
		return nil, 0, fmt.Errorf("telegram API error (%d): %s", tgResp.ErrorCode, tgResp.Description)
	}

	var msg Message
	if err := json.Unmarshal(tgResp.Result, &msg); err != nil {
		return nil, 0, fmt.Errorf("unmarshal message result: %w", err)
	}

	if msg.Document == nil {
		return nil, 0, errors.New("no document in telegram response")
	}

	return msg.Document, msg.MessageID, nil
}

// DownloadFile retrieves a file from Telegram by its FileID
func (c *Client) DownloadFile(ctx context.Context, fileID string) ([]byte, error) {
	_, token, err := c.pickBot()
	if err != nil {
		return nil, err
	}

	// 1. getFile
	getURL := fmt.Sprintf("%s/bot%s/getFile?file_id=%s", c.apiBaseURL(), token, fileID)
	req, err := http.NewRequestWithContext(ctx, "GET", getURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var tgResp apiResponse
	if err := json.Unmarshal(respBytes, &tgResp); err != nil {
		return nil, err
	}

	if !tgResp.OK {
		return nil, fmt.Errorf("getFile error (%d): %s", tgResp.ErrorCode, tgResp.Description)
	}

	var fileInfo TGFile
	if err := json.Unmarshal(tgResp.Result, &fileInfo); err != nil {
		return nil, err
	}

	// 2. Download bytes
	downloadURL := fmt.Sprintf("%s/file/bot%s/%s", c.apiBaseURL(), token, fileInfo.FilePath)
	dReq, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return nil, err
	}

	dResp, err := c.httpClient.Do(dReq)
	if err != nil {
		return nil, err
	}
	defer dResp.Body.Close()

	if dResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed with status %d", dResp.StatusCode)
	}

	var reader io.Reader = dResp.Body
	if c.throttler != nil {
		reader = qos.NewThrottledReader(reader, c.throttler)
	}

	return io.ReadAll(reader)
}

// SendAlert sends a notification message to the channel
func (c *Client) SendAlert(ctx context.Context, text string) error {
	token := c.PrimaryToken()
	if token == "" {
		var err error
		_, token, err = c.pickBot()
		if err != nil {
			return err
		}
	}

	formattedText := text
	if !strings.Contains(formattedText, "Backup") && !strings.Contains(formattedText, "[BACKUP]") {
		formattedText = "[BACKUP] " + formattedText
	}

	apiURL := fmt.Sprintf("%s/bot%s/sendMessage", c.apiBaseURL(), token)
	form := url.Values{}
	form.Set("chat_id", c.channelID)
	form.Set("text", formattedText)
	form.Set("parse_mode", "Markdown")

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// SetBaseURL overrides the API base URL (useful for test mocks)
func (c *Client) SetBaseURL(u string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseURL = u
}

// apiBaseURL returns the configured base URL or default
func (c *Client) apiBaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.baseURL != "" {
		return strings.TrimRight(c.baseURL, "/")
	}
	return apiBaseURL()
}

// PrimaryToken returns the primary bot token (index 0)
func (c *Client) PrimaryToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.tokens) == 0 {
		return ""
	}
	return c.tokens[0]
}

// GetMeshStatus returns status of all bots for the dashboard
func (c *Client) GetMeshStatus() []BotStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]BotStatus, 0, len(c.tokens))
	for i := 0; i < len(c.tokens); i++ {
		st := c.botStats[i]
		result = append(result, *st)
	}
	return result
}

// GetUpdates retrieves incoming updates using long-polling on the primary bot token
func (c *Client) GetUpdates(ctx context.Context, offset int64, limit int, timeout int) ([]Update, error) {
	token := c.PrimaryToken()
	if token == "" {
		return nil, errors.New("no bot token available for getUpdates")
	}

	if limit <= 0 {
		limit = 100
	}

	reqURL := fmt.Sprintf("%s/bot%s/getUpdates?offset=%d&limit=%d&timeout=%d", c.apiBaseURL(), token, offset, limit, timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var tgResp apiResponse
	if err := json.Unmarshal(respBytes, &tgResp); err != nil {
		return nil, fmt.Errorf("unmarshal telegram response: %w", err)
	}

	if !tgResp.OK {
		if tgResp.ErrorCode == 429 && tgResp.Parameters != nil {
			c.markRateLimited(0, tgResp.Parameters.RetryAfter)
			return nil, &RateLimitError{RetryAfter: tgResp.Parameters.RetryAfter}
		}
		return nil, fmt.Errorf("telegram getUpdates error (%d): %s", tgResp.ErrorCode, tgResp.Description)
	}

	var updates []Update
	if len(tgResp.Result) > 0 && string(tgResp.Result) != "null" {
		if err := json.Unmarshal(tgResp.Result, &updates); err != nil {
			return nil, fmt.Errorf("unmarshal updates: %w", err)
		}
	}

	return updates, nil
}

// SendMessageWithMarkup posts a message with optional inline keyboard to chatID
func (c *Client) SendMessageWithMarkup(ctx context.Context, chatID int64, text string, markup *InlineKeyboardMarkup) (*Message, error) {
	token := c.PrimaryToken()
	if token == "" {
		return nil, errors.New("no bot token available for sendMessage")
	}

	form := url.Values{}
	form.Set("chat_id", strconv.FormatInt(chatID, 10))
	form.Set("text", text)
	form.Set("parse_mode", "Markdown")
	if markup != nil {
		b, err := json.Marshal(markup)
		if err == nil {
			form.Set("reply_markup", string(b))
		}
	}

	reqURL := fmt.Sprintf("%s/bot%s/sendMessage", c.apiBaseURL(), token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var tgResp apiResponse
	if err := json.Unmarshal(respBytes, &tgResp); err != nil {
		return nil, fmt.Errorf("unmarshal telegram response: %w", err)
	}

	if !tgResp.OK {
		// Fallback without parse_mode if entity formatting error
		if strings.Contains(strings.ToLower(tgResp.Description), "parse") ||
			strings.Contains(strings.ToLower(tgResp.Description), "entity") {
			form.Del("parse_mode")
			fallbackReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(form.Encode()))
			if err == nil {
				fallbackReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if fallbackResp, err := c.httpClient.Do(fallbackReq); err == nil {
					defer fallbackResp.Body.Close()
					var fallbackTgResp apiResponse
					if err := json.NewDecoder(fallbackResp.Body).Decode(&fallbackTgResp); err == nil && fallbackTgResp.OK {
						var msg Message
						_ = json.Unmarshal(fallbackTgResp.Result, &msg)
						return &msg, nil
					}
				}
			}
		}
		return nil, fmt.Errorf("telegram sendMessage error (%d): %s", tgResp.ErrorCode, tgResp.Description)
	}

	var msg Message
	if err := json.Unmarshal(tgResp.Result, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal message: %w", err)
	}

	return &msg, nil
}

// EditMessageText modifies an existing message text and keyboard markup
func (c *Client) EditMessageText(ctx context.Context, chatID int64, messageID int64, text string, markup *InlineKeyboardMarkup) error {
	token := c.PrimaryToken()
	if token == "" {
		return errors.New("no bot token available for editMessageText")
	}

	form := url.Values{}
	form.Set("chat_id", strconv.FormatInt(chatID, 10))
	form.Set("message_id", strconv.FormatInt(messageID, 10))
	form.Set("text", text)
	form.Set("parse_mode", "Markdown")
	if markup != nil {
		b, err := json.Marshal(markup)
		if err == nil {
			form.Set("reply_markup", string(b))
		}
	}

	reqURL := fmt.Sprintf("%s/bot%s/editMessageText", c.apiBaseURL(), token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var tgResp apiResponse
	if err := json.Unmarshal(respBytes, &tgResp); err != nil {
		return fmt.Errorf("unmarshal telegram response: %w", err)
	}

	if !tgResp.OK {
		if strings.Contains(strings.ToLower(tgResp.Description), "parse") ||
			strings.Contains(strings.ToLower(tgResp.Description), "entity") {
			form.Del("parse_mode")
			fallbackReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(form.Encode()))
			if err == nil {
				fallbackReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if fallbackResp, err := c.httpClient.Do(fallbackReq); err == nil {
					defer fallbackResp.Body.Close()
					var fallbackTgResp apiResponse
					if err := json.NewDecoder(fallbackResp.Body).Decode(&fallbackTgResp); err == nil && fallbackTgResp.OK {
						return nil
					}
				}
			}
		}
		return fmt.Errorf("telegram editMessageText error (%d): %s", tgResp.ErrorCode, tgResp.Description)
	}

	return nil
}

// AnswerCallbackQuery answers an inline button callback query to dismiss client spinner
func (c *Client) AnswerCallbackQuery(ctx context.Context, callbackQueryID string, text string) error {
	token := c.PrimaryToken()
	if token == "" {
		return errors.New("no bot token available for answerCallbackQuery")
	}

	form := url.Values{}
	form.Set("callback_query_id", callbackQueryID)
	if text != "" {
		form.Set("text", text)
	}

	reqURL := fmt.Sprintf("%s/bot%s/answerCallbackQuery", c.apiBaseURL(), token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var tgResp apiResponse
	if err := json.Unmarshal(respBytes, &tgResp); err != nil {
		return fmt.Errorf("unmarshal telegram response: %w", err)
	}

	if !tgResp.OK {
		return fmt.Errorf("telegram answerCallbackQuery error (%d): %s", tgResp.ErrorCode, tgResp.Description)
	}

	return nil
}

// TestBotPing verifies bot token connectivity with Telegram API getMe
func TestBotPing(ctx context.Context, token string) (*User, error) {
	apiURL := fmt.Sprintf("%s/bot%s/getMe", apiBaseURL(), token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}
	if !apiResp.OK {
		return nil, fmt.Errorf("telegram error %d: %s", apiResp.ErrorCode, apiResp.Description)
	}

	var u User
	if err := json.Unmarshal(apiResp.Result, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

