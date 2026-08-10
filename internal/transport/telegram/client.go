package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Client sends messages via the Telegram Bot API.
type Client struct {
	token      string
	apiBaseURL string
	httpClient *http.Client

	// logger records the failures this client recovers from on its own.
	// Optional; nil means those recoveries stay silent, which is the
	// behaviour that hid a formatting regression for hours.
	logger *slog.Logger
}

// SetLogger attaches a logger. Without one, every fallback this client
// performs is invisible: the caller sees success and never learns the
// preferred path failed.
func (c *Client) SetLogger(l *slog.Logger) { c.logger = l }

// note records a recovered failure. Silent when no logger is attached.
func (c *Client) note(msg string, args ...any) {
	if c.logger == nil {
		return
	}
	c.logger.Warn(msg, args...)
}

// NewClient creates a new Telegram client.
func NewClient(token string, timeout time.Duration) *Client {
	return NewClientWithAPIURL(token, "", timeout)
}

// NewClientWithAPIURL creates a Telegram client backed by either the public
// Bot API or a self-hosted Bot API server.
func NewClientWithAPIURL(token, apiBaseURL string, timeout time.Duration) *Client {
	return &Client{
		token:      token,
		apiBaseURL: strings.TrimRight(strings.TrimSpace(apiBaseURL), "/"),
		httpClient: &http.Client{Timeout: timeout},
	}
}

func (c *Client) baseURL() string {
	if c.apiBaseURL != "" {
		return c.apiBaseURL
	}
	return "https://api.telegram.org"
}

func (c *Client) methodURL(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", c.baseURL(), c.token, method)
}

func (c *Client) fileURL(path string) string {
	return fmt.Sprintf("%s/file/bot%s/%s", c.baseURL(), c.token, strings.TrimLeft(path, "/"))
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
	u := c.methodURL("getMe")
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

// ResponseParameters carries structured recovery hints returned by the
// Telegram Bot API on failed requests.
type ResponseParameters struct {
	RetryAfter int `json:"retry_after,omitempty"`
}

// APIError is a Telegram-level failure. The Bot API often returns a useful
// JSON error body even when the HTTP request itself completed successfully,
// so callers must inspect ok/error_code rather than relying on HTTP alone.
type APIError struct {
	Method      string
	StatusCode  int
	ErrorCode   int
	Description string
	Parameters  ResponseParameters
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s failed (http=%d code=%d): %s",
		e.Method, e.StatusCode, e.ErrorCode, e.Description)
}

// SendMessage sends a text message to a chat.
func (c *Client) SendMessage(ctx context.Context, chatID string, text string) (*SendMessageResult, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("telegram bot not configured")
	}

	payload := map[string]any{
		"chat_id":    chatID,
		"text":       markdownToHTML(text),
		"parse_mode": "HTML",
	}
	result, err := c.postJSON(ctx, "sendMessage", payload)
	if err != nil && isEntityParseError(err) {
		// A malformed/incomplete Markdown fragment must never make the
		// transport lose the message. Retry as literal plain text — the
		// reader then sees raw markup, so say which message did it.
		c.note("telegram: markup rejected, resending as literal text",
			"chat_id", chatID, "error", err)
		payload["text"] = text
		delete(payload, "parse_mode")
		return c.postJSON(ctx, "sendMessage", payload)
	}
	return result, err
}

// InlineKeyboardButton is one button. Telegram requires exactly one
// optional field per button, so both are omitempty: a callback button
// carrying an empty "url", or a link button carrying an empty
// "callback_data", is rejected for the whole keyboard.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

// SendMessageWithKeyboard sends a message with an inline keyboard.
func (c *Client) SendMessageWithKeyboard(ctx context.Context, chatID int64, text string, rows [][]InlineKeyboardButton) (*SendMessageResult, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("telegram bot not configured")
	}
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       markdownToHTML(text),
		"parse_mode": "HTML",
		"reply_markup": map[string]any{
			"inline_keyboard": rows,
		},
	}
	return c.postJSON(ctx, "sendMessage", payload)
}

// EditMessageText edits an existing message's text and keyboard.
//
// Streaming reply paths (Codex / Gemini SSE) update one message in
// place via this method as new chunks arrive — so the same Markdown→HTML
// pre-processing the SendMessage path applies must apply here too,
// otherwise models that emit `**bold**` end up rendered as literal
// asterisks inside a parse_mode=HTML message (Telegram doesn't strip
// unrecognised markup, it shows it). Without this, formatting
// "intermittently broke depending on the model" — non-streaming
// providers used SendMessage and got conversion; streaming providers
// hit EditMessageText and didn't.
func (c *Client) EditMessageText(ctx context.Context, chatID int64, messageID int, text string, rows [][]InlineKeyboardButton) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       markdownToHTML(text),
		"parse_mode": "HTML",
	}
	if rows != nil {
		payload["reply_markup"] = map[string]any{"inline_keyboard": rows}
	} else {
		payload["reply_markup"] = map[string]any{"inline_keyboard": []any{}}
	}
	_, err := c.postJSON(ctx, "editMessageText", payload)
	if err != nil && isEntityParseError(err) {
		payload["text"] = text
		delete(payload, "parse_mode")
		_, err = c.postJSON(ctx, "editMessageText", payload)
	}
	return err
}

// EditMessageReplyMarkup replaces only the inline keyboard of an
// existing message — leaves the text body untouched. Used by the
// onboarding traits picker so each tap flips one button between
// "[ ] trait" and "[✓] trait" without re-sending the whole message
// (avoids chat clutter and keeps the same message_id for the next
// tap). Passing nil/empty rows clears the keyboard, matching the
// Telegram Bot API's editMessageReplyMarkup with an empty
// inline_keyboard array.
func (c *Client) EditMessageReplyMarkup(ctx context.Context, chatID int64, messageID int, rows [][]InlineKeyboardButton) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	if rows != nil {
		payload["reply_markup"] = map[string]any{"inline_keyboard": rows}
	} else {
		payload["reply_markup"] = map[string]any{"inline_keyboard": []any{}}
	}
	_, err := c.postJSON(ctx, "editMessageReplyMarkup", payload)
	return err
}

// AnswerCallbackQuery answers a callback query (removes loading spinner).
func (c *Client) AnswerCallbackQuery(ctx context.Context, callbackID string) error {
	return c.AnswerCallbackQueryText(ctx, callbackID, "")
}

// AnswerCallbackQueryText answers a callback query with a toast the user
// actually reads. Telegram shows the text as a notification above the chat,
// which is the only feedback available for a button whose effect (a turn
// stopping) takes a moment to become visible.
func (c *Client) AnswerCallbackQueryText(ctx context.Context, callbackID, text string) error {
	if !c.IsConfigured() {
		return nil
	}
	payload := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		payload["text"] = text
	}
	_, err := c.postJSON(ctx, "answerCallbackQuery", payload)
	return err
}

// DeleteMessage removes a message the bot sent. Used to clean up a turn
// placeholder that never became an answer, rather than leave a stray "…"
// in the chat.
func (c *Client) DeleteMessage(ctx context.Context, chatID int64, messageID int) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}
	_, err := c.postJSON(ctx, "deleteMessage", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	})
	return err
}

// BotCommand is one entry in the client's command menu — the list behind
// Telegram's "Menu" button and the autocomplete that opens on "/".
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SetMyCommands publishes this bot's command menu. An empty list clears
// it, which is Telegram's own semantics and the right outcome for a host
// that configures none.
//
// Telegram keeps this menu server-side and indefinitely: it survives our
// restarts, and a command left in it after we stop handling it goes on
// being offered forever. Publishing on every bot registration therefore
// makes the visible menu a function of the running config rather than of
// whatever was last set by hand in BotFather.
func (c *Client) SetMyCommands(ctx context.Context, commands []BotCommand) error {
	if !c.IsConfigured() {
		return nil
	}
	if commands == nil {
		commands = []BotCommand{}
	}
	_, err := c.postJSON(ctx, "setMyCommands", map[string]any{"commands": commands})
	return err
}

func (c *Client) postJSON(ctx context.Context, method string, payload map[string]any) (*SendMessageResult, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("telegram %s marshal request: %w", method, err)
	}
	apiURL := c.methodURL(method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, fmt.Errorf("telegram %s read response: %w", method, readErr)
	}
	var envelope struct {
		OK          bool               `json:"ok"`
		Description string             `json:"description,omitempty"`
		ErrorCode   int                `json:"error_code,omitempty"`
		Parameters  ResponseParameters `json:"parameters,omitempty"`
		Result      json.RawMessage    `json:"result,omitempty"`
	}
	decodeErr := json.Unmarshal(responseBody, &envelope)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices ||
		(decodeErr == nil && !envelope.OK) {
		description := envelope.Description
		if description == "" {
			description = strings.TrimSpace(string(responseBody))
		}
		if len(description) > 500 {
			description = description[:500] + "..."
		}
		return nil, &APIError{
			Method: method, StatusCode: resp.StatusCode, ErrorCode: envelope.ErrorCode,
			Description: description, Parameters: envelope.Parameters,
		}
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("telegram %s decode response: %w", method, decodeErr)
	}

	result := &SendMessageResult{OK: envelope.OK, Description: envelope.Description}
	trimmedResult := bytes.TrimSpace(envelope.Result)
	if len(trimmedResult) > 0 && trimmedResult[0] == '{' {
		if err := json.Unmarshal(trimmedResult, &result.Result); err != nil {
			return nil, fmt.Errorf("telegram %s decode message result: %w", method, err)
		}
	}
	return result, nil
}

func isEntityParseError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "can't parse entities")
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

	apiURL := c.methodURL("setMessageReaction")
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
	apiURL := c.methodURL("sendChatAction")
	_, err := c.httpClient.PostForm(apiURL, url.Values{
		"chat_id": {fmt.Sprintf("%d", chatID)},
		"action":  {action},
	})
	return err
}

// DownloadFile downloads a file by file_id and returns its content.
func (c *Client) DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, error) {
	path, cleanup, err := c.DownloadFilePath(ctx, fileID, maxBytes)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read telegram file: %w", err)
	}
	return data, nil
}

// DownloadFilePath resolves a Telegram file to a local path. A self-hosted
// Bot API server in --local mode returns the already-downloaded absolute path,
// so large videos never need to be copied into the daemon's memory. Public Bot
// API files are streamed into a temporary file and removed by cleanup.
func (c *Client) DownloadFilePath(ctx context.Context, fileID string, maxBytes int64) (path string, cleanup func(), err error) {
	if !c.IsConfigured() {
		return "", func() {}, fmt.Errorf("telegram bot not configured")
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}

	apiURL := c.methodURL("getFile") + "?file_id=" + url.QueryEscape(fileID)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", func() {}, err
	}
	fileClient := *c.httpClient
	if fileClient.Timeout < 15*time.Minute {
		fileClient.Timeout = 15 * time.Minute
	}
	resp, err := fileClient.Do(req)
	if err != nil {
		return "", func() {}, err
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
		return "", func() {}, err
	}
	if !result.OK || result.Result.FilePath == "" {
		return "", func() {}, fmt.Errorf("getFile failed for %s", fileID)
	}
	if result.Result.FileSize > maxBytes {
		return "", func() {}, fmt.Errorf("file too large: %d bytes (max %d)", result.Result.FileSize, maxBytes)
	}

	if filepath.IsAbs(result.Result.FilePath) {
		info, statErr := os.Stat(result.Result.FilePath)
		if statErr != nil {
			return "", func() {}, fmt.Errorf("stat telegram local file: %w", statErr)
		}
		if info.Size() > maxBytes {
			return "", func() {}, fmt.Errorf("file too large: %d bytes (max %d)", info.Size(), maxBytes)
		}
		return result.Result.FilePath, func() {}, nil
	}

	dlURL := c.fileURL(result.Result.FilePath)
	dlReq, err := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
	if err != nil {
		return "", func() {}, err
	}
	dlResp, err := fileClient.Do(dlReq)
	if err != nil {
		return "", func() {}, err
	}
	defer dlResp.Body.Close()

	tmp, err := os.CreateTemp("", "blueship-telegram-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create telegram temp file: %w", err)
	}
	tmpPath := tmp.Name()
	remove := func() { _ = os.Remove(tmpPath) }
	written, copyErr := io.Copy(tmp, io.LimitReader(dlResp.Body, maxBytes+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		remove()
		return "", func() {}, fmt.Errorf("download telegram file: %w", copyErr)
	}
	if closeErr != nil {
		remove()
		return "", func() {}, fmt.Errorf("close telegram temp file: %w", closeErr)
	}
	if written > maxBytes {
		remove()
		return "", func() {}, fmt.Errorf("file too large: more than %d bytes", maxBytes)
	}
	return tmpPath, remove, nil
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

	apiURL := c.methodURL("sendVoice")
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

// SendDocument sends a file (bytes) as a Telegram document. mime is
// the source Content-Type; empty defaults to application/octet-stream
// so a renamed binary doesn't get rendered as text. The Telegram bot
// API doesn't strictly require the form-field content type — it
// sniffs from the bytes — but setting it correctly lets the
// recipient client pick the right preview affordance.
func (c *Client) SendDocument(ctx context.Context, chatID string, filename, mime string, data []byte) error {
	mime, data = prepareTextDocument(filename, mime, data)
	return c.sendFile(ctx, chatID, "sendDocument", "document", filename, mime, data, nil)
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// prepareTextDocument makes Telegram/iOS document previews decode textual
// attachments as UTF-8. Telegram preserves the multipart MIME type, but some
// clients still guess a legacy code page for .md/.txt files without a BOM,
// turning Cyrillic into mojibake such as "РњРѕ...".
//
// Apply this only at the Telegram delivery boundary: stored attachment bytes
// stay clean UTF-8 for editing, hashing, and other transports. Binary documents
// and invalid UTF-8 text are left byte-for-byte unchanged.
func prepareTextDocument(filename, contentType string, data []byte) (string, []byte) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "text/") || !utf8.Valid(data) {
		return contentType, data
	}
	if params == nil {
		params = map[string]string{}
	}
	params["charset"] = "utf-8"
	contentType = mime.FormatMediaType(mediaType, params)
	if bytes.HasPrefix(data, utf8BOM) || !telegramPreviewNeedsBOM(filename, mediaType) {
		return contentType, data
	}
	out := make([]byte, 0, len(utf8BOM)+len(data))
	out = append(out, utf8BOM...)
	out = append(out, data...)
	return contentType, out
}

func telegramPreviewNeedsBOM(filename, mediaType string) bool {
	if strings.EqualFold(mediaType, "text/markdown") {
		return true
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".md", ".markdown", ".txt", ".csv", ".log":
		return true
	default:
		return false
	}
}

// SendPhoto sends an image as a Telegram photo — gets the inline
// gallery preview, swipe-to-zoom, etc., as opposed to the file-icon
// affordance SendDocument produces. Falls back to SendDocument when
// the image is too large for the photo endpoint (TG caps photos at
// 10 MB whereas documents go to 50 MB) so a 12 MB PNG from the
// cabinet still reaches the user. `caption` is optional; pass empty
// for a bare photo send.
func (c *Client) SendPhoto(ctx context.Context, chatID string, filename, mime string, data []byte, caption string) error {
	if len(data) > 10*1024*1024 {
		return c.SendDocument(ctx, chatID, filename, mime, data)
	}
	extra := map[string]string{}
	if caption != "" {
		extra["caption"] = caption
	}
	return c.sendFile(ctx, chatID, "sendPhoto", "photo", filename, mime, data, extra)
}

// sendFile is the multipart-upload core shared by SendDocument and
// SendPhoto. method is the Telegram API method ("sendDocument" /
// "sendPhoto"), field is the form-field name TG expects for the
// payload ("document" / "photo"), and extra carries any non-binary
// fields (e.g. caption).
func (c *Client) sendFile(ctx context.Context, chatID, method, field, filename, mime string, data []byte, extra map[string]string) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}
	if mime == "" {
		mime = "application/octet-stream"
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("chat_id", chatID)
	for k, v := range extra {
		_ = w.WriteField(k, v)
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filename))
	h.Set("Content-Type", mime)
	part, err := w.CreatePart(h)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", field, err)
	}
	w.Close()

	apiURL := c.methodURL(method)
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
		return fmt.Errorf("%s: status %d: %s", method, resp.StatusCode, string(errBody))
	}
	return nil
}

const maxTelegramMessageLength = 4096

// SendLong sends a long text message, splitting into chunks if needed.
func (c *Client) SendLong(ctx context.Context, chatID int64, text string) error {
	if len([]rune(text)) <= maxTelegramMessageLength {
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
	if maxLen <= 0 {
		return nil
	}
	runes := []rune(text)
	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= maxLen {
			chunks = append(chunks, string(runes))
			break
		}

		cutAt := maxLen
		singleNewline := 0
		for i := maxLen; i > maxLen/4; i-- {
			if i >= 2 && runes[i-1] == '\n' && runes[i-2] == '\n' {
				cutAt = i
				break
			}
			if runes[i-1] == '\n' && singleNewline == 0 {
				singleNewline = i
			}
		}
		if cutAt == maxLen && singleNewline != 0 {
			cutAt = singleNewline
		}

		chunks = append(chunks, string(runes[:cutAt]))
		runes = runes[cutAt:]
	}
	return chunks
}

var (
	reCodeBlock = regexp.MustCompile("(?s)```(?:\\w*\n)?(.*?)```")
	reInline    = regexp.MustCompile("`([^`]+)`")
	reBold      = regexp.MustCompile(`\*\*(.+?)\*\*`)
	// LaTeX-style math leaks from streaming providers (Codex / Gemini
	// occasionally emit `$\rightarrow$`, `$\alpha$`, `$x^2$` instead of
	// using Unicode). Telegram doesn't render LaTeX, so the raw form
	// shows up as `$\rightarrow$` in the chat. Strip the wrappers and
	// substitute the most common arrow / inequality symbols. We don't
	// try to be a full LaTeX parser — just a few high-frequency offenders
	// that show up when models pretend they're writing for arxiv.
	reLatexInline = regexp.MustCompile(`\$([^$\n]{1,400})\$`)
	// Inside `$...$`: rewrite the most common LaTeX structural commands
	// to a readable plain-text form. Handled in this order:
	//   \frac{a}{b}  →  (a/b)
	//   \text{xyz} / \mathrm{xyz} / \mathbf{xyz} / \mathit{xyz} →  xyz
	//   _{xyz}        →  _xyz   (subscript without braces)
	//   ^{xyz}        →  ^xyz   (superscript without braces)
	// Everything else (single-token commands like \sigma, \rightarrow)
	// goes through latexToUnicode below. What stays as `\foo` after both
	// passes is a long-tail command we don't render — better than
	// leaving the whole `$…$` block intact.
	reLatexFrac        = regexp.MustCompile(`\\frac\s*\{([^{}]+)\}\s*\{([^{}]+)\}`)
	reLatexBraced      = regexp.MustCompile(`\\(?:text|mathrm|mathbf|mathit|operatorname)\s*\{([^{}]+)\}`)
	reLatexSubscript   = regexp.MustCompile(`_\{([^{}]+)\}`)
	reLatexSuperscript = regexp.MustCompile(`\^\{([^{}]+)\}`)
	// Strip residual single-token LaTeX commands the unicode map didn't
	// catch (e.g. `\textbf` mid-string after \text already ran). Without
	// this they'd render verbatim. Conservative: only strips commands
	// followed by space/punct/end so we don't eat backslash-escaped
	// regex content elsewhere.
	reLatexResidual = regexp.MustCompile(`\\[a-zA-Z]+\b`)
	// Subword tokenization glitches: a model occasionally emits
	// `Princ\_iple` (escaped underscore mid-word) when it streams a long
	// English word — we strip that escape so the word reads cleanly.
	// Only mid-word `\_`, leave standalone `\_` alone.
	reEscapedUnderscoreMidWord = regexp.MustCompile(`(\w)\\_(\w)`)
)

// latexToUnicode maps the LaTeX commands that show up most often in
// model output to their Unicode equivalents. Anything not in the map
// is dropped along with the surrounding `$…$` — it would render as
// raw LaTeX otherwise. Extend this list when new offenders surface;
// no need for full LaTeX coverage.
var latexToUnicode = strings.NewReplacer(
	`\rightarrow`, `→`,
	`\Rightarrow`, `⇒`,
	`\leftarrow`, `←`,
	`\Leftarrow`, `⇐`,
	`\leftrightarrow`, `↔`,
	`\to`, `→`,
	`\gets`, `←`,
	`\geq`, `≥`,
	`\leq`, `≤`,
	`\neq`, `≠`,
	`\approx`, `≈`,
	`\times`, `×`,
	`\cdot`, `·`,
	`\pm`, `±`,
	`\infty`, `∞`,
	`\alpha`, `α`,
	`\beta`, `β`,
	`\gamma`, `γ`,
	`\delta`, `δ`,
	`\lambda`, `λ`,
	`\mu`, `μ`,
	`\sigma`, `σ`,
	`\pi`, `π`,
	`\theta`, `θ`,
	`\Delta`, `Δ`,
	`\Sigma`, `Σ`,
	`\Omega`, `Ω`,
)

func markdownToHTML(text string) string {
	// Pre-pass 1: rewrite LaTeX inside `$...$` wrappers to a plain-text
	// approximation, then drop the wrappers themselves. Done BEFORE HTML
	// escaping so substituted Unicode (→, ≥, σ, etc.) survives untouched.
	// Order matters: structural commands (\frac, \text) first, then
	// subscript/superscript braces, then unicode token map, then strip
	// residual long-tail commands.
	text = reLatexInline.ReplaceAllStringFunc(text, func(match string) string {
		inner := match[1 : len(match)-1]
		inner = reLatexFrac.ReplaceAllString(inner, "($1/$2)")
		inner = reLatexBraced.ReplaceAllString(inner, "$1")
		inner = reLatexSubscript.ReplaceAllString(inner, "_$1")
		inner = reLatexSuperscript.ReplaceAllString(inner, "^$1")
		inner = latexToUnicode.Replace(inner)
		inner = reLatexResidual.ReplaceAllString(inner, "")
		return inner
	})
	// Pre-pass 2: heal mid-word escaped underscores ("Princ\_iple" → "Principle").
	text = reEscapedUnderscoreMidWord.ReplaceAllString(text, `$1$2`)

	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")

	text = reCodeBlock.ReplaceAllString(text, "<pre>$1</pre>")
	text = reInline.ReplaceAllString(text, "<code>$1</code>")
	text = reBold.ReplaceAllString(text, "<b>$1</b>")

	return text
}
