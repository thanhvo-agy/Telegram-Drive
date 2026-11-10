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
	"sync"
	"sync/atomic"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/qos"
)

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
	MessageID int64     `json:"message_id"`
	Date      int64     `json:"date"`
	Document  *Document `json:"document,omitempty"`
	Caption   string    `json:"caption,omitempty"`
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

	reqURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", token)
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
	getURL := fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", token, fileID)
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
	downloadURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", token, fileInfo.FilePath)
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
	_, token, err := c.pickBot()
	if err != nil {
		return err
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	payload := map[string]interface{}{
		"chat_id":    c.channelID,
		"text":       text,
		"parse_mode": "Markdown",
	}

	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
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

// TestBotPing verifies bot token connectivity with Telegram API getMe
func TestBotPing(ctx context.Context, token string) (*User, error) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", token)
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
