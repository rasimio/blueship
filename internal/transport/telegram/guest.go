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
	"strconv"
	"strings"
)

// maxCaptionLength is Telegram's limit on a media caption.
const maxCaptionLength = 1024

// AnswerGuestQuery issues the one reply a guest summons allows and returns
// the id of the inline message it became.
//
// Telegram gives a guest bot a single message per summons, so everything
// after this call — the streamed answer, the final render — is an edit of
// that message. There is no second send to fall back on.
func (c *Client) AnswerGuestQuery(ctx context.Context, guestQueryID, text string) (string, error) {
	if !c.IsConfigured() {
		return "", fmt.Errorf("telegram bot not configured")
	}
	content := map[string]any{
		"message_text": markdownToHTML(text),
		"parse_mode":   "HTML",
	}
	payload := map[string]any{
		"guest_query_id": guestQueryID,
		"result": map[string]any{
			"type":                  "article",
			"id":                    "answer",
			"title":                 "answer",
			"input_message_content": content,
		},
	}
	res, err := c.postJSON(ctx, "answerGuestQuery", payload)
	if err != nil && isEntityParseError(err) {
		c.note("telegram: markup rejected in a guest answer, resending as literal text", "error", err)
		content["message_text"] = text
		delete(content, "parse_mode")
		res, err = c.postJSON(ctx, "answerGuestQuery", payload)
	}
	if err != nil {
		return "", err
	}
	var sent struct {
		InlineMessageID string `json:"inline_message_id"`
	}
	if err := json.Unmarshal(res.Raw, &sent); err != nil || sent.InlineMessageID == "" {
		return "", fmt.Errorf("telegram answerGuestQuery: no inline_message_id in %s", string(res.Raw))
	}
	return sent.InlineMessageID, nil
}

// EditInlineMessageText replaces the text of an inline message — a guest
// answer. Same rendering and markup fallback as EditMessageText; an edit
// that changes nothing is not an error.
func (c *Client) EditInlineMessageText(ctx context.Context, inlineMessageID, text string) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}
	payload := map[string]any{
		"inline_message_id": inlineMessageID,
		"text":              markdownToHTML(text),
		"parse_mode":        "HTML",
	}
	_, err := c.postJSON(ctx, "editMessageText", payload)
	if err != nil && isEntityParseError(err) {
		payload["text"] = text
		delete(payload, "parse_mode")
		_, err = c.postJSON(ctx, "editMessageText", payload)
	}
	if isMessageNotModified(err) {
		return nil
	}
	return err
}

// EditInlineRichMessage turns an inline message into a Rich Message.
func (c *Client) EditInlineRichMessage(ctx context.Context, inlineMessageID, text string) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}
	_, err := c.postJSON(ctx, "editMessageText", map[string]any{
		"inline_message_id": inlineMessageID,
		"rich_message": InputRichMessage{
			Markdown: prepareRichMarkdown(text),
		},
	})
	if isMessageNotModified(err) {
		return nil
	}
	return err
}

// FinalizeInlineResponse writes a finished answer into a guest reply.
//
// It is FinalizeResponse without the overflow path: an inline message
// cannot be followed by more messages, so an answer too long for it is
// cut with an ellipsis rather than continued below. Rich rendering takes
// what a plain message cannot hold — tables, code, anything past 4096
// characters — and plain text is the fallback when rich is refused.
func (c *Client) FinalizeInlineResponse(ctx context.Context, inlineMessageID, text string) error {
	if !NeedsRich(text) && len([]rune(text)) <= maxTelegramMessageLength {
		return retryTelegram(ctx, func() error {
			return c.EditInlineMessageText(ctx, inlineMessageID, text)
		})
	}
	rich := cutWithEllipsis(text, maxTelegramRichChunkLength)
	richErr := retryTelegram(ctx, func() error {
		return c.EditInlineRichMessage(ctx, inlineMessageID, rich)
	})
	if richErr == nil {
		return nil
	}
	c.note("telegram: rich edit of a guest answer rejected, finalising as plain text", "error", richErr)
	plain := cutWithEllipsis(text, maxTelegramMessageLength)
	if err := retryTelegram(ctx, func() error {
		return c.EditInlineMessageText(ctx, inlineMessageID, plain)
	}); err != nil {
		return fmt.Errorf("finalize guest answer: rich edit: %v; plain fallback: %w", richErr, err)
	}
	return nil
}

// cutWithEllipsis bounds text to limit runes, marking the cut. Runes, not
// bytes: a byte cut lands mid-codepoint on any non-ASCII text.
func cutWithEllipsis(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}

// InlineMedia is a file already on Telegram's servers, which is the only
// kind of file an inline message can be given. Kind is "photo" or
// "document".
type InlineMedia struct {
	Kind   string
	FileID string
}

// UploadForInline puts a file on Telegram's servers so an inline message can
// carry it, and reports the message it had to become to get there.
//
// An inline message takes no uploads, only file ids, and the one way a bot
// gets a file id is to send the file to a chat. chatID is that chat; the
// send is silent, and the caller deletes the message once the inline
// message holds the file.
func (c *Client) UploadForInline(ctx context.Context, chatID int64, kind, filename, mime string, data []byte) (InlineMedia, int, error) {
	if !c.IsConfigured() {
		return InlineMedia{}, 0, fmt.Errorf("telegram bot not configured")
	}
	method, field := "sendDocument", "document"
	if kind == "photo" {
		method, field = "sendPhoto", "photo"
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	_ = w.WriteField("disable_notification", "true")
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filename))
	h.Set("Content-Type", mime)
	part, err := w.CreatePart(h)
	if err != nil {
		return InlineMedia{}, 0, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return InlineMedia{}, 0, fmt.Errorf("write %s: %w", field, err)
	}
	w.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(method), &body)
	if err != nil {
		return InlineMedia{}, 0, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return InlineMedia{}, 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return InlineMedia{}, 0, fmt.Errorf("telegram %s read response: %w", method, err)
	}
	var envelope struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int         `json:"message_id"`
			Photo     []PhotoSize `json:"photo"`
			Document  *Document   `json:"document"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil || !envelope.OK {
		return InlineMedia{}, 0, fmt.Errorf("telegram %s failed (http=%d): %s", method, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	messageID := envelope.Result.MessageID
	switch {
	case kind == "photo" && len(envelope.Result.Photo) > 0:
		largest := envelope.Result.Photo[len(envelope.Result.Photo)-1]
		return InlineMedia{Kind: "photo", FileID: largest.FileID}, messageID, nil
	case envelope.Result.Document != nil && envelope.Result.Document.FileID != "":
		return InlineMedia{Kind: "document", FileID: envelope.Result.Document.FileID}, messageID, nil
	}
	// Sent, but with no file id there is nothing to put in the inline
	// message; the message still has to be cleaned up.
	return InlineMedia{}, messageID, fmt.Errorf("telegram %s: no file id in the result", method)
}

// EditInlineMedia turns an inline message into a media message — a text
// message included — with caption under it.
func (c *Client) EditInlineMedia(ctx context.Context, inlineMessageID string, media InlineMedia, caption string) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}
	input := map[string]any{"type": media.Kind, "media": media.FileID}
	if caption != "" {
		input["caption"] = markdownToHTML(caption)
		input["parse_mode"] = "HTML"
	}
	payload := map[string]any{"inline_message_id": inlineMessageID, "media": input}
	_, err := c.postJSON(ctx, "editMessageMedia", payload)
	if err != nil && isEntityParseError(err) {
		input["caption"] = caption
		delete(input, "parse_mode")
		_, err = c.postJSON(ctx, "editMessageMedia", payload)
	}
	if isMessageNotModified(err) {
		return nil
	}
	return err
}

// editInlineRichWithMedia makes an inline message a Rich Message with the
// files embedded after the text, in order.
func (c *Client) editInlineRichWithMedia(ctx context.Context, inlineMessageID, text string, media []InlineMedia) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}
	var md strings.Builder
	md.WriteString(prepareRichMarkdown(text))
	items := make([]map[string]any, 0, len(media))
	for i, m := range media {
		id := "m" + strconv.Itoa(i+1)
		md.WriteString("\n\n![](tg://" + m.Kind + "?id=" + id + ")")
		items = append(items, map[string]any{
			"id":    id,
			"media": map[string]any{"type": m.Kind, "media": m.FileID},
		})
	}
	_, err := c.postJSON(ctx, "editMessageText", map[string]any{
		"inline_message_id": inlineMessageID,
		"rich_message": map[string]any{
			"markdown": md.String(),
			"media":    items,
		},
	})
	if isMessageNotModified(err) {
		return nil
	}
	return err
}

// FinalizeInlineWithMedia writes a finished answer that carries files into a
// guest reply.
//
// One file under an answer short enough for a caption goes in the way
// Telegram shows a picture with words: a media message, its caption
// copyable. Anything more is a Rich Message with the files embedded. If rich
// is refused, the first file still goes in with the answer cut to a caption:
// the file is what was asked for, and the text survives in the history.
func (c *Client) FinalizeInlineWithMedia(ctx context.Context, inlineMessageID, text string, media []InlineMedia) error {
	if len(media) == 0 {
		return c.FinalizeInlineResponse(ctx, inlineMessageID, text)
	}
	if len(media) == 1 && len([]rune(text)) <= maxCaptionLength {
		return retryTelegram(ctx, func() error {
			return c.EditInlineMedia(ctx, inlineMessageID, media[0], text)
		})
	}
	rich := cutWithEllipsis(text, maxTelegramRichChunkLength-200*len(media))
	richErr := retryTelegram(ctx, func() error {
		return c.editInlineRichWithMedia(ctx, inlineMessageID, rich, media)
	})
	if richErr == nil {
		return nil
	}
	c.note("telegram: rich guest answer with files rejected, sending the first file with a cut caption", "error", richErr)
	caption := cutWithEllipsis(text, maxCaptionLength)
	if err := retryTelegram(ctx, func() error {
		return c.EditInlineMedia(ctx, inlineMessageID, media[0], caption)
	}); err != nil {
		return fmt.Errorf("finalize guest answer with files: rich edit: %v; media fallback: %w", richErr, err)
	}
	return nil
}
