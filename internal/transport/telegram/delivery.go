package telegram

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// Telegram Rich Messages accept up to 32768 UTF-8 characters. Keep a
	// little headroom for server-side parsing/normalisation and split only
	// unusually large reports.
	maxTelegramRichChunkLength = 32000
	finalDeliveryAttempts      = 3
)

// InputRichMessage is the transport shape accepted by sendRichMessage and by
// editMessageText's rich_message field. Raw model Markdown is intentional:
// Telegram Rich Markdown is GFM-compatible and natively renders headings,
// nested lists, tables, formulas, details and URL-backed media blocks.
type InputRichMessage struct {
	Markdown            string `json:"markdown"`
	SkipEntityDetection bool   `json:"skip_entity_detection,omitempty"`
}

var (
	richDollarMathBlock  = regexp.MustCompile(`(?ms)^[ \t]*\$\$[ \t]*\n?(.*?)\n?[ \t]*\$\$[ \t]*$`)
	richBracketMathBlock = regexp.MustCompile(`(?ms)^[ \t]*\\\[[ \t]*\n?(.*?)\n?[ \t]*\\\][ \t]*$`)
)

func prepareRichMarkdown(text string) string {
	// Rich Markdown specifies $...$ for inline formulas and the
	// tg-math-block tag for display formulas. Models commonly emit the
	// equivalent $$...$$ or \[...\] forms, so normalise those two block
	// spellings at the transport boundary.
	text = richDollarMathBlock.ReplaceAllString(text, "<tg-math-block>$1</tg-math-block>")
	return richBracketMathBlock.ReplaceAllString(text, "<tg-math-block>$1</tg-math-block>")
}

// SendRichMessage sends a persistent Telegram Rich Message.
func (c *Client) SendRichMessage(ctx context.Context, chatID int64, text string) (*SendMessageResult, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("telegram bot not configured")
	}
	return c.postJSON(ctx, "sendRichMessage", map[string]any{
		"chat_id": chatID,
		"rich_message": InputRichMessage{
			Markdown: prepareRichMarkdown(text),
		},
	})
}

// EditRichMessage replaces an existing preview with the final rich report.
func (c *Client) EditRichMessage(ctx context.Context, chatID int64, messageID int, text string, rows [][]InlineKeyboardButton) error {
	if !c.IsConfigured() {
		return fmt.Errorf("telegram bot not configured")
	}
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"rich_message": InputRichMessage{
			Markdown: prepareRichMarkdown(text),
		},
	}
	if rows != nil {
		payload["reply_markup"] = map[string]any{"inline_keyboard": rows}
	} else {
		payload["reply_markup"] = map[string]any{"inline_keyboard": []any{}}
	}
	_, err := c.postJSON(ctx, "editMessageText", payload)
	return err
}

// SendRichLong delivers arbitrary Markdown as one or more Rich Messages and
// falls back to ordinary Telegram text chunks when rich parsing is rejected.
func (c *Client) SendRichLong(ctx context.Context, chatID int64, text string) error {
	for _, chunk := range splitMessage(text, maxTelegramRichChunkLength) {
		if err := c.sendRichWithFallback(ctx, chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// FinalizeResponse is the authoritative Telegram delivery step. Progressive
// edits are only a preview; this method must still deliver the complete reply
// when the preview edit failed, the response exceeds a classic 4096-character
// message, or Telegram temporarily rejects a request.
func (c *Client) FinalizeResponse(ctx context.Context, chatID int64, previewMessageID int, text string) error {
	chunks := splitMessage(text, maxTelegramRichChunkLength)
	for i, chunk := range chunks {
		if i == 0 && previewMessageID != 0 {
			richEditErr := retryTelegram(ctx, func() error {
				return c.EditRichMessage(ctx, chatID, previewMessageID, chunk, nil)
			})
			if richEditErr == nil {
				continue
			}

			// Compatibility fallback for an older Bot API endpoint or a rich
			// parser rejection. Keep the existing bubble when ordinary text
			// editing still works; if the message itself is no longer editable,
			// send a fresh rich/plain message below.
			if err := c.finalizeLegacyPreview(ctx, chatID, previewMessageID, chunk); err == nil {
				continue
			}
			if err := c.sendRichWithFallback(ctx, chatID, chunk); err != nil {
				return fmt.Errorf("finalize telegram preview: rich edit: %v; fallback: %w", richEditErr, err)
			}
			continue
		}

		if err := c.sendRichWithFallback(ctx, chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) finalizeLegacyPreview(ctx context.Context, chatID int64, messageID int, text string) error {
	chunks := splitMessage(text, maxTelegramMessageLength)
	if len(chunks) == 0 {
		return nil
	}
	if err := retryTelegram(ctx, func() error {
		return c.EditMessageText(ctx, chatID, messageID, chunks[0], nil)
	}); err != nil {
		return err
	}
	for _, chunk := range chunks[1:] {
		if err := c.sendLegacyWithRetry(ctx, chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) sendRichWithFallback(ctx context.Context, chatID int64, text string) error {
	var richResult *SendMessageResult
	richErr := retryTelegram(ctx, func() error {
		var err error
		richResult, err = c.SendRichMessage(ctx, chatID, text)
		return err
	})
	if richErr == nil && richResult != nil && richResult.Result.MessageID != 0 {
		return nil
	}

	for _, chunk := range splitMessage(text, maxTelegramMessageLength) {
		if err := c.sendLegacyWithRetry(ctx, chatID, chunk); err != nil {
			return fmt.Errorf("send rich message: %v; plain fallback: %w", richErr, err)
		}
	}
	return nil
}

func (c *Client) sendLegacyWithRetry(ctx context.Context, chatID int64, text string) error {
	return retryTelegram(ctx, func() error {
		_, err := c.SendMessage(ctx, fmt.Sprintf("%d", chatID), text)
		return err
	})
}

func retryTelegram(ctx context.Context, operation func() error) error {
	var lastErr error
	for attempt := 0; attempt < finalDeliveryAttempts; attempt++ {
		if err := operation(); err != nil {
			if isMessageNotModified(err) {
				return nil
			}
			lastErr = err
			if !isRetryableTelegramError(err) || attempt == finalDeliveryAttempts-1 {
				return err
			}
		} else {
			return nil
		}

		delay := time.Duration(250*(1<<attempt)) * time.Millisecond
		var apiErr *APIError
		if errors.As(lastErr, &apiErr) && apiErr.Parameters.RetryAfter > 0 {
			delay = time.Duration(apiErr.Parameters.RetryAfter) * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

func isRetryableTelegramError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode == 429 || apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
	}
	// Transport/decode failures are transient. Editing is idempotent, and
	// final sends prefer at-least-once delivery over silently losing a reply.
	return true
}

func isMessageNotModified(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "message is not modified")
}
