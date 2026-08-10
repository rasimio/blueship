package gateway

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// telegramPreviewEditInterval throttles progressive edits. Telegram rate-limits
// edits per chat; 600ms reads as live typing without tripping it.
const telegramPreviewEditInterval = 600 * time.Millisecond

// telegramPreviewClient is the slice of the Telegram client a preview needs.
// Narrow on purpose: it makes the preview testable without a bot token, and
// it documents that a preview only ever touches one message.
type telegramPreviewClient interface {
	SendMessageWithKeyboard(ctx context.Context, chatID int64, text string, rows [][]telegram.InlineKeyboardButton) (*telegram.SendMessageResult, error)
	EditMessageText(ctx context.Context, chatID int64, messageID int, text string, rows [][]telegram.InlineKeyboardButton) error
	FinalizeResponse(ctx context.Context, chatID int64, previewMessageID int, text string) error
	DeleteMessage(ctx context.Context, chatID int64, messageID int) error
}

// telegramPreview is the single Telegram message one turn writes itself into:
// created when the answer starts arriving, edited as text and tool status
// come in, and finalised in place as the answer. The stop control rides it
// and disappears with the finished answer.
//
// It posts nothing before there is something to say. An earlier version put a
// placeholder up at turn start so the stop button existed during the pause
// before the first word; what that produced was a chat full of hourglasses,
// and — worse — a message per cancelled turn, since a placeholder that never
// grew still had to be disposed of somehow. Nothing to show means no message.
// The gap is covered by /stop, which needs no message to exist.
type telegramPreview struct {
	client   telegramPreviewClient
	chatID   int64
	logger   *slog.Logger
	keyboard [][]telegram.InlineKeyboardButton

	mu         sync.Mutex
	messageID  int
	buf        strings.Builder
	toolStatus string
	lastEdit   time.Time
	failed     bool // log the first transport failure, not every delta
	settled    bool // finalised or abandoned; no further edits
}

func newTelegramPreview(
	client telegramPreviewClient,
	chatID int64,
	logger *slog.Logger,
	keyboard [][]telegram.InlineKeyboardButton,
) *telegramPreview {
	return &telegramPreview{client: client, chatID: chatID, logger: logger, keyboard: keyboard}
}

// ensureLocked creates the message on the first thing worth showing. Failure
// is not fatal: without a preview the turn still runs and still delivers its
// answer as a fresh message, it just loses the in-place typing effect.
func (p *telegramPreview) ensureLocked(ctx context.Context, text string) {
	if p.messageID != 0 || p.settled || p.client == nil {
		return
	}
	body := telegramPreviewText(text, "")
	if body == "" {
		return
	}
	res, err := p.client.SendMessageWithKeyboard(ctx, p.chatID, body, p.keyboard)
	if err != nil {
		if !p.failed {
			p.failed = true
			p.logger.Warn("telegram preview create failed", "chat_id", p.chatID, "error", err)
		}
		return
	}
	if res == nil || res.Result.MessageID == 0 {
		return
	}
	p.messageID = res.Result.MessageID
	p.lastEdit = time.Now()
}

// telegramPreviewMinCreateBytes is how much text has to exist before the
// message is worth creating. Posting on the very first delta would put a
// single letter on screen and then grow it, which reads as a glitch.
const telegramPreviewMinCreateBytes = 10

// appendText adds one streamed chunk and refreshes the message, throttled.
func (p *telegramPreview) appendText(ctx context.Context, chunk string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf.WriteString(chunk)
	if text := strings.TrimSpace(p.buf.String()); len(text) > telegramPreviewMinCreateBytes {
		p.ensureLocked(ctx, text)
	}
	p.flushLocked(ctx)
}

// noteTool shows which tool is running, so a turn that spends thirty seconds
// in browser_fetch doesn't look stuck.
func (p *telegramPreview) noteTool(ctx context.Context, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.toolStatus = ">> " + name + "..."
	p.ensureLocked(ctx, "`"+p.toolStatus+"`")
	p.flushLocked(ctx)
}

func (p *telegramPreview) flushLocked(ctx context.Context) {
	if p.messageID == 0 || p.settled || p.client == nil {
		return
	}
	if time.Since(p.lastEdit) < telegramPreviewEditInterval {
		return
	}
	text := telegramPreviewText(p.buf.String(), p.toolStatus)
	if text == "" {
		return
	}
	// The keyboard goes out on every edit: Telegram treats an edit without
	// reply_markup as "remove the keyboard", so omitting it here would drop
	// the stop button on the first streamed chunk.
	if err := p.client.EditMessageText(ctx, p.chatID, p.messageID, text, p.keyboard); err != nil && !p.failed {
		p.failed = true
		p.logger.Warn("telegram preview edit failed",
			"chat_id", p.chatID, "message_id", p.messageID, "error", err)
	}
	p.lastEdit = time.Now()
}

// clearTool drops the tool status line before the final render.
func (p *telegramPreview) clearTool() {
	p.mu.Lock()
	p.toolStatus = ""
	p.mu.Unlock()
}

// hasClient reports whether this preview can talk to Telegram at all. A turn
// on any other transport carries an inert preview so the call sites stay free
// of nil checks.
func (p *telegramPreview) hasClient() bool { return p != nil && p.client != nil }

// messageIDForDelivery returns the message the answer should be written into,
// or 0 when there is none (the placeholder never posted) and the answer has to
// go out as a fresh message.
func (p *telegramPreview) messageIDForDelivery() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.messageID
}

// finalize renders the finished answer into the preview and drops the stop
// control with it — FinalizeResponse edits without a keyboard, which is how
// Telegram removes one.
func (p *telegramPreview) finalize(ctx context.Context, text string) error {
	p.mu.Lock()
	messageID := p.messageID
	p.settled = true
	p.mu.Unlock()
	return p.client.FinalizeResponse(ctx, p.chatID, messageID, text)
}

// settle closes out a preview whose turn ended without delivering a final
// answer — stopped by the reader, cut off by a restart, failed.
//
// It never creates a message and never writes words of its own. Announcing
// the interruption in the chat is what produced replies the model had no
// record of giving.
//
// recorded says whether what streamed reached the transcript. When it did,
// the message stays as the reader saw it, minus the stop control, which has
// nothing left to stop. When it did not — a turn cut off before it had even
// persisted the question it was answering — the message goes, because a reply
// on screen that exists nowhere else is the thing being fixed here. That case
// is a cancel within a second or two of asking, so there is next to nothing on
// screen to lose.
func (p *telegramPreview) settle(ctx context.Context, recorded bool) {
	p.mu.Lock()
	if p.settled || p.messageID == 0 || p.client == nil {
		p.mu.Unlock()
		return
	}
	p.settled = true
	messageID := p.messageID
	partial := strings.TrimSpace(p.buf.String())
	p.mu.Unlock()

	if partial == "" || !recorded {
		if err := p.client.DeleteMessage(ctx, p.chatID, messageID); err != nil {
			p.logger.Warn("telegram preview delete failed",
				"chat_id", p.chatID, "message_id", messageID, "error", err)
		}
		return
	}
	// One last edit so the message shows everything that streamed — the
	// live edits are throttled, so the tail chunk may not be on screen —
	// and drops the keyboard with it.
	if err := p.client.FinalizeResponse(ctx, p.chatID, messageID, partial); err != nil {
		p.logger.Warn("telegram preview settle failed",
			"chat_id", p.chatID, "message_id", messageID, "error", err)
	}
}

// newTurnPreview builds the preview a turn writes into: a live Telegram
// message for the Telegram transport, an inert one everywhere else. Nothing is
// posted yet — the message appears with the first thing worth reading.
func (g *Gateway) newTurnPreview(sink bs.ResponseSink, turnID string) *telegramPreview {
	tgSink, ok := sink.(*telegramSink)
	if !ok || tgSink.client == nil {
		return &telegramPreview{logger: g.logger}
	}
	return newTelegramPreview(
		tgSink.client,
		tgSink.chatID,
		g.logger,
		stopKeyboard(turnID, g.deps.Config.UI.StopButton),
	)
}

// stopKeyboard is the one-button keyboard a preview carries. The turn id
// travels in the callback data so a tap that lands after this turn ended
// cannot stop the next one; "stop:" + a uuid is 41 bytes, inside Telegram's
// 64-byte callback_data limit.
func stopKeyboard(turnID, label string) [][]telegram.InlineKeyboardButton {
	return [][]telegram.InlineKeyboardButton{{
		{Text: label, CallbackData: stopCallbackPrefix + turnID},
	}}
}

const stopCallbackPrefix = "stop:"
