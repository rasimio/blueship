package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Client sends messages via the Telegram Bot API.
type Client struct {
	token      string
	httpClient *http.Client
}

// NewClient creates a new Telegram client.
func NewClient(token string, timeout time.Duration) *Client {
	return &Client{
		token:      token,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// IsConfigured returns true if the client has a bot token.
func (c *Client) IsConfigured() bool {
	return c.token != ""
}

// BotIdentity is the Telegram self-identity used by the gateway for routing
// in group chats: resolving "@<botname>" mentions, detecting replies to us
// vs replies to other chat members, and targeted slash commands.
type BotIdentity struct {
	ID       int64
	Username string
}

// GetMe returns the bot's ID and username (from Telegram /getMe). The gateway
// caches the result at startup to route incoming group-chat messages without
// hitting the Bot API on every update.
func (c *Client) GetMe(ctx context.Context) (BotIdentity, error) {
	if !c.IsConfigured() {
		return BotIdentity{}, fmt.Errorf("telegram bot not configured")
	}
	u := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", c.token)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return BotIdentity{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return BotIdentity{}, err
	}
	defer resp.Body.Close()
	var body struct {
		OK     bool `json:"ok"`
		Result struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return BotIdentity{}, err
	}
	if !body.OK {
		return BotIdentity{}, fmt.Errorf("getMe not ok")
	}
	return BotIdentity{ID: body.Result.ID, Username: body.Result.Username}, nil
}

// SendMessageResult is the Telegram API response for sendMessage.
type SendMessageResult struct {
	OK          bool   `json:"ok"`
	Description string `json:"description,omitempty"`
	Result      struct {
		MessageID int `json:"message_id"`
	} `json:"result"`
}

// SendMessage sends a text message to a chat.
func (c *Client) SendMessage(ctx context.Context, chatID string, text string) (*SendMessageResult, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("telegram bot not configured")
	}

	htmlText := markdownToHTML(text)

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", c.token)

	resp, err := c.httpClient.PostForm(apiURL, url.Values{
		"chat_id":    {chatID},
		"text":       {htmlText},
		"parse_mode": {"HTML"},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result SendMessageResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if !result.OK {
		if strings.Contains(result.Description, "can't parse entities") {
			resp2, err := c.httpClient.PostForm(apiURL, url.Values{
				"chat_id": {chatID},
				"text":    {text},
			})
			if err != nil {
				return nil, err
			}
			defer resp2.Body.Close()

			var result2 SendMessageResult
			if err := json.NewDecoder(resp2.Body).Decode(&result2); err != nil {
				return nil, err
			}
			if !result2.OK {
				return nil, fmt.Errorf("telegram API error: %s", result2.Description)
			}
			return &result2, nil
		}
		return nil, fmt.Errorf("telegram API error: %s", result.Description)
	}

	return &result, nil
}

// InlineKeyboardButton represents one button in an inline keyboard.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// SendMessageWithKeyboard sends a message with an inline keyboard.
func (c *Client) SendMessageWithKeyboard(ctx context.Context, chatID int64, text string, rows [][]InlineKeyboardButton) (*SendMessageResult, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("telegram bot not configured")
	}
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
		"reply_markup": map[string]any{
			"inline_keyboard": rows,
		},
	}
	return c.postJSON(ctx, "sendMessage", payload)
}

// EditMessageText edits an existing message's text and keyboard.
func (c *Client) EditMessageText(ctx context.Context, chatID int64, messageID int, text string, rows [][]InlineKeyboardButton) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "HTML",
	}
	if rows != nil {
		payload["reply_markup"] = map[string]any{"inline_keyboard": rows}
	} else {
		payload["reply_markup"] = map[string]any{"inline_keyboard": []any{}}
	}
	_, err := c.postJSON(ctx, "editMessageText", payload)
	return err
}

// AnswerCallbackQuery answers a callback query (removes loading spinner).
func (c *Client) AnswerCallbackQuery(ctx context.Context, callbackID string) error {
	if !c.IsConfigured() {
		return nil
	}
	payload := map[string]any{"callback_query_id": callbackID}
	_, err := c.postJSON(ctx, "answerCallbackQuery", payload)
	return err
}

func (c *Client) postJSON(ctx context.Context, method string, payload map[string]any) (*SendMessageResult, error) {
	body, _ := json.Marshal(payload)
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/%s", c.token, method)
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result SendMessageResult
	json.NewDecoder(resp.Body).Decode(&result)
	return &result, nil
}

// SetReaction sets an emoji reaction on a message.
func (c *Client) SetReaction(ctx context.Context, chatID int64, messageID int, emoji string) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}

	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"reaction":   []map[string]string{{"type": "emoji", "emoji": emoji}},
	}
	body, _ := json.Marshal(payload)

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/setMessageReaction", c.token)
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(body))
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

// SendChatAction sends a chat action (e.g. "typing").
func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	if !c.IsConfigured() {
		return nil
	}
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendChatAction", c.token)
	_, err := c.httpClient.PostForm(apiURL, url.Values{
		"chat_id": {fmt.Sprintf("%d", chatID)},
		"action":  {action},
	})
	return err
}

// DownloadFile downloads a file by file_id and returns its content.
func (c *Client) DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("telegram bot not configured")
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", c.token, url.QueryEscape(fileID))
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
			FileSize int64  `json:"file_size"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.OK || result.Result.FilePath == "" {
		return nil, fmt.Errorf("getFile failed for %s", fileID)
	}
	if result.Result.FileSize > maxBytes {
		return nil, fmt.Errorf("file too large: %d bytes (max %d)", result.Result.FileSize, maxBytes)
	}

	dlURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", c.token, result.Result.FilePath)
	dlReq, err := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
	if err != nil {
		return nil, err
	}
	dlResp, err := c.httpClient.Do(dlReq)
	if err != nil {
		return nil, err
	}
	defer dlResp.Body.Close()

	return io.ReadAll(io.LimitReader(dlResp.Body, maxBytes))
}

// SendVoice sends an OGG Opus voice message to a chat.
func (c *Client) SendVoice(ctx context.Context, chatID string, audio []byte) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("chat_id", chatID)
	part, err := w.CreateFormFile("voice", "voice.ogg")
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return fmt.Errorf("write audio: %w", err)
	}
	w.Close()

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendVoice", c.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sendVoice: status %d: %s", resp.StatusCode, string(errBody))
	}
	return nil
}

// SendDocument sends a file (bytes) as a Telegram document.
func (c *Client) SendDocument(ctx context.Context, chatID string, filename string, data []byte) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("chat_id", chatID)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="document"; filename="%s"`, filename))
	h.Set("Content-Type", "text/plain; charset=utf-8")
	part, err := w.CreatePart(h)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("write document: %w", err)
	}
	w.Close()

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", c.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sendDocument: status %d: %s", resp.StatusCode, string(errBody))
	}
	return nil
}

const maxTelegramMessageLength = 4096

// SendLong sends a long text message, splitting into chunks if needed.
func (c *Client) SendLong(ctx context.Context, chatID int64, text string) error {
	if len(text) <= maxTelegramMessageLength {
		_, err := c.SendMessage(ctx, fmt.Sprintf("%d", chatID), text)
		return err
	}

	chunks := splitMessage(text, maxTelegramMessageLength)
	for _, chunk := range chunks {
		if _, err := c.SendMessage(ctx, fmt.Sprintf("%d", chatID), chunk); err != nil {
			return err
		}
	}
	return nil
}

func splitMessage(text string, maxLen int) []string {
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		cutAt := maxLen
		if idx := strings.LastIndex(text[:maxLen], "\n\n"); idx > maxLen/4 {
			cutAt = idx + 2
		} else if idx := strings.LastIndex(text[:maxLen], "\n"); idx > maxLen/4 {
			cutAt = idx + 1
		}

		chunks = append(chunks, strings.TrimRight(text[:cutAt], "\n"))
		text = text[cutAt:]
	}
	return chunks
}

var (
	reCodeBlock = regexp.MustCompile("(?s)```(?:\\w*\n)?(.*?)```")
	reInline    = regexp.MustCompile("`([^`]+)`")
	reBold      = regexp.MustCompile(`\*\*(.+?)\*\*`)
)

func markdownToHTML(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")

	text = reCodeBlock.ReplaceAllString(text, "<pre>$1</pre>")
	text = reInline.ReplaceAllString(text, "<code>$1</code>")
	text = reBold.ReplaceAllString(text, "<b>$1</b>")

	return text
}
