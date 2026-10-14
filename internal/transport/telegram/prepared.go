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
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

// PreparedMessage is a message its sender composed in full: Telegram HTML
// exactly as given — no Markdown is read into it — an optional photo, and
// its keyboard. For mailings, whose text was written and checked in
// advance and has to arrive the way it was checked.
type PreparedMessage struct {
	ChatID int64
	// HTML is the message text, or the photo's caption when there is one.
	HTML string
	// PhotoFileID resends a photo this bot has uploaded before; Photo
	// uploads one. At most one of them is set.
	PhotoFileID string
	Photo       *PhotoUpload
	Rows        [][]InlineKeyboardButton
	// Silent delivers without a sound (disable_notification).
	Silent bool
}

// PhotoUpload is the bytes of a photo sent for the first time.
type PhotoUpload struct {
	Name string
	MIME string
	Data []byte
}

// PreparedResult is what Telegram reported for a delivered message.
type PreparedResult struct {
	MessageID int
	// PhotoFileID is the id of the delivered photo's largest size. A later
	// message from the same bot can send it again without uploading it.
	PhotoFileID string
}

// TransportError is a request Telegram gave no answer to. Written says
// whether the request left this process in full: when it did not,
// Telegram cannot have acted on it; when it did, it may have, and only
// the chat can tell. Its text never names the request URL, which carries
// the bot token.
type TransportError struct {
	Method  string
	Written bool
	Err     error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("telegram %s: %v", e.Method, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// SendPrepared delivers msg with exactly one request. It never retries
// and never falls back to another form: after a lost answer there is no
// telling a lost send from a lost reply, and the caller is the one that
// decides what an ambiguous outcome means. Failures are an *APIError when
// Telegram answered, a *TransportError when it did not.
func (c *Client) SendPrepared(ctx context.Context, msg PreparedMessage) (PreparedResult, error) {
	if !c.IsConfigured() {
		return PreparedResult{}, fmt.Errorf("telegram bot not configured")
	}
	if msg.PhotoFileID != "" && msg.Photo != nil {
		return PreparedResult{}, fmt.Errorf("telegram: a prepared message carries one photo, not two")
	}
	fields := map[string]string{
		"chat_id":    strconv.FormatInt(msg.ChatID, 10),
		"parse_mode": "HTML",
	}
	if msg.Silent {
		fields["disable_notification"] = "true"
	}
	if len(msg.Rows) > 0 {
		markup, err := json.Marshal(map[string]any{"inline_keyboard": msg.Rows})
		if err != nil {
			return PreparedResult{}, fmt.Errorf("telegram: encode keyboard: %w", err)
		}
		fields["reply_markup"] = string(markup)
	}

	method := "sendMessage"
	switch {
	case msg.Photo != nil:
		method = "sendPhoto"
		fields["caption"] = msg.HTML
	case msg.PhotoFileID != "":
		method = "sendPhoto"
		fields["caption"] = msg.HTML
		fields["photo"] = msg.PhotoFileID
	default:
		fields["text"] = msg.HTML
		// The admin preview shows no link card, so neither does the chat.
		fields["link_preview_options"] = `{"is_disabled":true}`
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return PreparedResult{}, fmt.Errorf("telegram: write %s: %w", k, err)
		}
	}
	if msg.Photo != nil {
		mimeType := msg.Photo.MIME
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		name := msg.Photo.Name
		if name == "" {
			name = "photo"
		}
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="photo"; filename=%q`, name))
		h.Set("Content-Type", mimeType)
		part, err := w.CreatePart(h)
		if err != nil {
			return PreparedResult{}, fmt.Errorf("telegram: create photo part: %w", err)
		}
		if _, err := part.Write(msg.Photo.Data); err != nil {
			return PreparedResult{}, fmt.Errorf("telegram: write photo: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return PreparedResult{}, fmt.Errorf("telegram: close form: %w", err)
	}

	if err := c.pace(ctx, method); err != nil {
		return PreparedResult{}, &TransportError{Method: method, Err: err}
	}
	var written atomic.Bool
	traced := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				written.Store(true)
			}
		},
	})
	req, err := http.NewRequestWithContext(traced, http.MethodPost, c.methodURL(method), &body)
	if err != nil {
		return PreparedResult{}, &TransportError{Method: method, Err: errors.New("build request")}
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return PreparedResult{}, &TransportError{Method: method, Written: written.Load(), Err: withoutURL(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return PreparedResult{}, &TransportError{Method: method, Written: true, Err: fmt.Errorf("read response: %w", withoutURL(err))}
	}

	var envelope struct {
		OK          bool               `json:"ok"`
		Description string             `json:"description"`
		ErrorCode   int                `json:"error_code"`
		Parameters  ResponseParameters `json:"parameters"`
		Result      struct {
			MessageID int `json:"message_id"`
			Photo     []struct {
				FileID string `json:"file_id"`
			} `json:"photo"`
		} `json:"result"`
	}
	decodeErr := json.Unmarshal(raw, &envelope)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && decodeErr == nil && envelope.OK {
		out := PreparedResult{MessageID: envelope.Result.MessageID}
		if n := len(envelope.Result.Photo); n > 0 {
			out.PhotoFileID = envelope.Result.Photo[n-1].FileID
		}
		return out, nil
	}
	if decodeErr != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// An answer nobody can read: the message may well be in the chat.
		return PreparedResult{}, &TransportError{Method: method, Written: true, Err: errors.New("unreadable response")}
	}
	description := envelope.Description
	if description == "" {
		description = http.StatusText(resp.StatusCode)
	}
	if len(description) > 500 {
		description = description[:500] + "..."
	}
	if envelope.ErrorCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusTooManyRequests {
		c.limiter.pause(time.Duration(envelope.Parameters.RetryAfter) * time.Second)
	}
	return PreparedResult{}, &APIError{
		Method: method, StatusCode: resp.StatusCode, ErrorCode: envelope.ErrorCode,
		Description: description, Parameters: envelope.Parameters,
	}
}

// withoutURL drops the request URL from a transport error: it carries the
// bot token, and these errors are logged and stored.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}
