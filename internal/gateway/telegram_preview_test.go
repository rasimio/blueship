package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rasimio/blueship/internal/transport/telegram"
)

// fakePreviewClient records what a preview does to Telegram.
type fakePreviewClient struct {
	sent      []string
	sentRows  [][][]telegram.InlineKeyboardButton
	edits     []string
	editRows  [][][]telegram.InlineKeyboardButton
	finalized []string
	deleted   []int
	sendErr   error
}

func (f *fakePreviewClient) SendMessageWithKeyboard(_ context.Context, _ int64, text string, rows [][]telegram.InlineKeyboardButton) (*telegram.SendMessageResult, error) {
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.sent = append(f.sent, text)
	f.sentRows = append(f.sentRows, rows)
	res := &telegram.SendMessageResult{OK: true}
	res.Result.MessageID = 4242
	return res, nil
}

func (f *fakePreviewClient) EditMessageText(_ context.Context, _ int64, _ int, text string, rows [][]telegram.InlineKeyboardButton) error {
	f.edits = append(f.edits, text)
	f.editRows = append(f.editRows, rows)
	return nil
}

func (f *fakePreviewClient) FinalizeResponse(_ context.Context, _ int64, _ int, text string) error {
	f.finalized = append(f.finalized, text)
	return nil
}

func (f *fakePreviewClient) DeleteMessage(_ context.Context, _ int64, messageID int) error {
	f.deleted = append(f.deleted, messageID)
	return nil
}

func newTestPreview(client telegramPreviewClient) *telegramPreview {
	return newTelegramPreview(client, 77, slog.New(slog.DiscardHandler),
		stopKeyboard("turn-1", "⏹ Stop"))
}

// Variant B of the design: the message exists from the moment the turn does,
// so the wait before the first token is stoppable. Posting it only once text
// arrives would leave the stop button missing during exactly the pause people
// want to interrupt.
func TestPreviewPostsTheStopControlBeforeAnyText(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)

	p.start(context.Background(), "⌛")

	if len(client.sent) != 1 || client.sent[0] != "⌛" {
		t.Fatalf("placeholder sent = %+v", client.sent)
	}
	if len(client.sentRows) != 1 || len(client.sentRows[0]) != 1 {
		t.Fatalf("placeholder posted without a stop control: %+v", client.sentRows)
	}
	if got := client.sentRows[0][0][0].CallbackData; got != stopCallbackPrefix+"turn-1" {
		t.Fatalf("stop button callback = %q", got)
	}
}

// Telegram reads an edit with no reply_markup as "remove the keyboard", so a
// preview that forgets to re-send it loses the stop button on the first chunk
// of streamed text — precisely when the answer gets long enough to want out.
func TestPreviewKeepsTheStopControlThroughEdits(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)
	p.start(context.Background(), "⌛")
	p.lastEdit = time.Now().Add(-time.Minute) // past the edit throttle

	p.appendText(context.Background(), "the first half")

	if len(client.edits) != 1 {
		t.Fatalf("edits = %+v", client.edits)
	}
	if len(client.editRows[0]) != 1 || len(client.editRows[0][0]) != 1 {
		t.Fatal("streamed edit dropped the stop control")
	}
}

// What the user was reading stays on screen, and it says where it stopped —
// the same marker the transcript gets, so chat and history agree.
func TestPreviewSettlesAnInterruptedAnswerWithItsPartialText(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)
	p.start(context.Background(), "⌛")
	p.lastEdit = time.Now().Add(-time.Minute)
	p.appendText(context.Background(), "half an answer")

	p.settle(context.Background(), "[interrupted]", " […interrupted]")

	if len(client.finalized) != 1 || client.finalized[0] != "half an answer […interrupted]" {
		t.Fatalf("finalized = %+v", client.finalized)
	}
	if len(client.deleted) != 0 {
		t.Fatal("an answer with text was deleted instead of kept")
	}
}

// Stopped before the first word: the tap needs a visible result, or the chat
// shows a question that looks ignored.
func TestPreviewSettlesAnEmptyInterruptedTurnWithTheMarker(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)
	p.start(context.Background(), "⌛")

	p.settle(context.Background(), "[interrupted]", " […interrupted]")

	if len(client.finalized) != 1 || client.finalized[0] != "[interrupted]" {
		t.Fatalf("finalized = %+v", client.finalized)
	}
}

// A turn that failed or answered with nothing leaves no ghost behind.
func TestPreviewDeletesAnEmptyPlaceholderWhenTheTurnJustFailed(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)
	p.start(context.Background(), "⌛")

	p.settle(context.Background(), "", "")

	if len(client.deleted) != 1 || client.deleted[0] != 4242 {
		t.Fatalf("deleted = %+v", client.deleted)
	}
	if len(client.finalized) != 0 {
		t.Fatalf("empty failed turn was finalized as an answer: %+v", client.finalized)
	}
}

// settle runs from a defer on every path, including the one where the answer
// was already delivered. Touching the message again there would overwrite the
// reply with a stale partial.
func TestPreviewSettleIsANoOpAfterDelivery(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)
	p.start(context.Background(), "⌛")
	if err := p.finalize(context.Background(), "the whole answer"); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	p.settle(context.Background(), "[interrupted]", " […interrupted]")

	if len(client.finalized) != 1 || client.finalized[0] != "the whole answer" {
		t.Fatalf("finalized = %+v", client.finalized)
	}
	if len(client.deleted) != 0 {
		t.Fatal("a delivered answer was deleted")
	}
}

// Telegram being briefly unavailable costs the typing effect, not the turn.
func TestPreviewSurvivesAFailedPlaceholder(t *testing.T) {
	client := &fakePreviewClient{sendErr: errors.New("429 too many requests")}
	p := newTestPreview(client)

	p.start(context.Background(), "⌛")
	p.appendText(context.Background(), "text nobody can preview")
	p.settle(context.Background(), "[interrupted]", " […interrupted]")

	if p.messageIDForDelivery() != 0 {
		t.Fatal("preview claims a message id it never got")
	}
	if len(client.edits) != 0 || len(client.finalized) != 0 || len(client.deleted) != 0 {
		t.Fatalf("preview edited a message that was never created: %+v", client)
	}
}

// A non-Telegram transport carries an inert preview so the turn code needs no
// nil checks; it must stay silent rather than panic.
func TestInertPreviewDoesNothing(t *testing.T) {
	p := &telegramPreview{logger: slog.New(slog.DiscardHandler)}

	p.start(context.Background(), "⌛")
	p.appendText(context.Background(), "text")
	p.noteTool(context.Background(), "browser_fetch")
	p.clearTool()
	p.settle(context.Background(), "[interrupted]", " […interrupted]")

	if p.hasClient() {
		t.Fatal("inert preview claims a client")
	}
}

func TestTelegramPreviewTextCapsUnicodeAndKeepsToolStatus(t *testing.T) {
	status := ">> browser_fetch..."
	preview := telegramPreviewText(strings.Repeat("я", 5000), status)
	if n := utf8.RuneCountInString(preview); n != telegramPreviewMaxRunes {
		t.Fatalf("preview runes = %d, want %d", n, telegramPreviewMaxRunes)
	}
	if !strings.HasSuffix(preview, "`"+status+"`") {
		t.Fatalf("preview lost tool status: %q", preview[len(preview)-40:])
	}
}

func TestTelegramPreviewTextWithToolOnly(t *testing.T) {
	if got := telegramPreviewText("", ">> notes..."); got != "`>> notes...`" {
		t.Fatalf("tool-only preview = %q", got)
	}
}
