package telegram

import (
	"context"
	"encoding/json"
	"fmt"
)

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
