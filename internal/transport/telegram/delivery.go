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
	richFencedBlock      = regexp.MustCompile("(?ms)(^|\\n)[ \\t]*```[ \\t]*\\r?\\n(.*?)\\r?\\n[ \\t]*```[ \\t]*(\\r?\\n|$)")
	richTableDividerCell = regexp.MustCompile(`^:?-{3,}:?$`)
)

func prepareRichMarkdown(text string) string {
	text = richFencedBlock.ReplaceAllStringFunc(text, normalizeFencedPipeTable)

	// Rich Markdown specifies $...$ for inline formulas and the
	// tg-math-block tag for display formulas. Models commonly emit the
	// equivalent $$...$$ or \[...\] forms, so normalise those two block
	// spellings at the transport boundary.
	text = richDollarMathBlock.ReplaceAllString(text, "<tg-math-block>$1</tg-math-block>")
	return richBracketMathBlock.ReplaceAllString(text, "<tg-math-block>$1</tg-math-block>")
}

func normalizeFencedPipeTable(block string) string {
	leadingNewline := strings.HasPrefix(block, "\n")
	trailingNewline := strings.HasSuffix(block, "\n")

	trimmed := strings.TrimSuffix(strings.TrimPrefix(block, "\n"), "\n")
	lines := strings.Split(trimmed, "\n")
	if len(lines) < 5 { // opening fence + header + divider + data + closing fence
		return block
	}
	body := lines[1 : len(lines)-1]
	rows := make([][]string, 0, len(body))
	for _, line := range body {
		row := splitPipeTableRow(line)
		if len(row) < 2 || (len(rows) > 0 && len(row) != len(rows[0])) {
			return block
		}
		rows = append(rows, row)
	}
	for _, cell := range rows[1] {
		if !richTableDividerCell.MatchString(cell) {
			return block
		}
	}

	var normalized strings.Builder
	if leadingNewline {
		normalized.WriteByte('\n')
	}
	for i, row := range rows {
		if i > 0 {
			normalized.WriteByte('\n')
		}
		normalized.WriteString("| ")
		normalized.WriteString(strings.Join(row, " | "))
		normalized.WriteString(" |")
	}
	if trailingNewline {
		normalized.WriteByte('\n')
	}
	return normalized.String()
}

func splitPipeTableRow(line string) []string {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	if !strings.Contains(line, "|") {
		return nil
	}
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
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

// NeedsRich reports whether a message actually requires a Rich Message to
// render — a table, a fenced code block, or a display formula.
//
// Everything else goes as an ordinary Telegram message, and that is not a
// cosmetic choice: text inside a Rich Message cannot be selected and
// copied out of the chat. Sending every reply as one made a bot whose
// answers you can read and cannot use — and the ordinary path renders
// bold, italics and links perfectly well, as copyable entities.
//
// Deliberately narrow. Headings and bullet lists are not on this list:
// they render as ordinary text well enough, and prose in a chat should
// not be a document anyway.
func NeedsRich(text string) bool {
	if strings.Contains(text, "```") {
		return true
	}
	if strings.Contains(text, "<tg-math-block>") ||
		richDollarMathBlock.MatchString(text) || richBracketMathBlock.MatchString(text) {
		return true
	}
	return hasPipeTable(text)
}

// hasPipeTable looks for a Markdown table: a header row followed by a
// divider. A lone pipe is not enough — prose uses them, and "a | b" is
// not a table.
func hasPipeTable(text string) bool {
	lines := strings.Split(text, "\n")
	for i := 0; i+1 < len(lines); i++ {
		header := strings.TrimSpace(lines[i])
		divider := strings.TrimSpace(lines[i+1])
		if !strings.Contains(header, "|") || !strings.Contains(divider, "|") {
			continue
		}
		cells := splitPipeTableRow(divider)
		if len(cells) < 2 {
			continue
		}
		allDividers := true
		for _, cell := range cells {
			if !richTableDividerCell.MatchString(strings.TrimSpace(cell)) {
				allDividers = false
				break
			}
		}
		if allDividers {
			return true
		}
	}
	return false
}

// SendRichLong delivers Markdown to a chat.
//
// Rich Messages only when the content needs them; ordinary messages
// otherwise, so the text can be copied. Falls back to ordinary text
// chunks when rich parsing is rejected.
func (c *Client) SendRichLong(ctx context.Context, chatID int64, text string) error {
	if !NeedsRich(text) && len([]rune(text)) <= maxTelegramMessageLength {
		return c.SendLong(ctx, chatID, text)
	}
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
	// This is the path a chat answer actually takes, and it is where the
	// reply stops being copyable: the streamed preview is an ordinary
	// message, and finalising swapped it for a Rich one. Only content that
	// needs rich rendering gets it; the rest stays an ordinary message,
	// which Telegram lets a reader select and copy.
	//
	// Above the ordinary message limit rich wins anyway: Telegram caps a
	// normal message at 4096 characters and a rich one at 32000, so the
	// alternative is scattering one answer across ten bubbles. Something
	// that long is a document already, and the complaint this fixes is
	// about ordinary replies.
	if !NeedsRich(text) && len([]rune(text)) <= maxTelegramMessageLength {
		if previewMessageID != 0 {
			return c.finalizeLegacyPreview(ctx, chatID, previewMessageID, text)
		}
		return c.SendLong(ctx, chatID, text)
	}

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
			//
			// Recorded first: succeeding here leaves the reader looking at
			// the plain renderer's output, which drops every table the
			// model wrote, and the caller is told the finalize succeeded.
			c.note("telegram: rich edit rejected, finalising as plain text",
				"chat_id", chatID, "message_id", previewMessageID, "error", richEditErr)
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

	// The plain path below renders none of what rich renders — no tables,
	// no headings, no italics. Silently swapping one for the other is
	// indistinguishable from "formatting stopped working", with nothing
	// anywhere to say why, so the discarded error is recorded before the
	// fallback hides it.
	c.note("telegram: rich message rejected, falling back to plain text",
		"chat_id", chatID, "error", richErr)

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
