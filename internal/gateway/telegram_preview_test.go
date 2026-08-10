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

// Nothing to say, nothing on screen. A placeholder posted at turn start put
// an hourglass in the chat on every single turn, and left a stray message
// behind every time a turn was cancelled before it said anything.
func TestPreviewPostsNothingBeforeThereIsSomethingToRead(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)

	p.appendText(context.Background(), "hi") // below the create threshold
	p.settle(context.Background(), true)

	if len(client.sent) != 0 {
		t.Fatalf("posted a message with nothing to show: %+v", client.sent)
	}
	if len(client.finalized) != 0 || len(client.deleted) != 0 {
		t.Fatalf("touched a message that was never created: %+v", client)
	}
}

// The answer carries its own stop control, and it appears as soon as there is
// an answer to stop.
func TestPreviewCreatesTheMessageWithTheStopControl(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)

	p.appendText(context.Background(), "the answer starts here")

	if len(client.sent) != 1 {
		t.Fatalf("sent = %+v", client.sent)
	}
	if len(client.sentRows) != 1 || len(client.sentRows[0]) != 1 {
		t.Fatalf("message posted without a stop control: %+v", client.sentRows)
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
	p.appendText(context.Background(), "the answer starts here")
	p.lastEdit = time.Now().Add(-time.Minute) // past the edit throttle

	p.appendText(context.Background(), " and continues")

	if len(client.edits) != 1 {
		t.Fatalf("edits = %+v", client.edits)
	}
	if len(client.editRows[0]) != 1 || len(client.editRows[0][0]) != 1 {
		t.Fatal("streamed edit dropped the stop control")
	}
}

// A stopped answer keeps what the reader was reading, and loses the control
// that has nothing left to stop. It says nothing extra: the interruption is
// not written to the transcript when there was nothing to answer, so words
// added here would exist on screen and nowhere else.
func TestPreviewSettleKeepsThePartialAnswerAndAddsNothing(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)
	p.appendText(context.Background(), "half an answer")

	p.settle(context.Background(), true)

	if len(client.finalized) != 1 || client.finalized[0] != "half an answer" {
		t.Fatalf("finalized = %+v", client.finalized)
	}
	if len(client.sent) != 1 {
		t.Fatalf("settle posted an extra message: %+v", client.sent)
	}
	if len(client.deleted) != 0 {
		t.Fatal("an answer with text was deleted instead of kept")
	}
}

// The regression this replaced: a turn stopped before it said anything used
// to leave "[interrupted by user]" sitting in the chat as if it were a reply,
// while the transcript — correctly — recorded nothing of the sort.
func TestPreviewSettleAnnouncesNothingWhenNothingWasSaid(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)

	p.settle(context.Background(), true)

	if len(client.sent) != 0 || len(client.finalized) != 0 {
		t.Fatalf("a cancelled turn left a message behind: sent=%+v finalized=%+v",
			client.sent, client.finalized)
	}
}

// The other half of the same rule: text that reached the screen but not the
// transcript is taken back off the screen. Otherwise the reader is left with
// a reply the assistant has no record of giving — and will contradict.
func TestPreviewRemovesAnAnswerThatWasNeverRecorded(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)
	p.appendText(context.Background(), "half an answer nobody stored")

	p.settle(context.Background(), false)

	if len(client.deleted) != 1 || client.deleted[0] != 4242 {
		t.Fatalf("deleted = %+v", client.deleted)
	}
	if len(client.finalized) != 0 {
		t.Fatalf("an unrecorded answer was left on screen: %+v", client.finalized)
	}
}

// A message that got created for a tool status and never grew past it is
// removed rather than left as a ghost.
func TestPreviewDeletesAToolOnlyMessageWithNoAnswer(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)
	p.noteTool(context.Background(), "browser_fetch")

	p.settle(context.Background(), true)

	if len(client.deleted) != 1 || client.deleted[0] != 4242 {
		t.Fatalf("deleted = %+v", client.deleted)
	}
	if len(client.finalized) != 0 {
		t.Fatalf("empty turn was finalized as an answer: %+v", client.finalized)
	}
}

// settle runs from a defer on every path, including the one where the answer
// was already delivered. Touching the message again there would overwrite the
// reply with a stale partial.
func TestPreviewSettleIsANoOpAfterDelivery(t *testing.T) {
	client := &fakePreviewClient{}
	p := newTestPreview(client)
	p.appendText(context.Background(), "the answer starts here")
	if err := p.finalize(context.Background(), "the whole answer"); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	p.settle(context.Background(), true)

	if len(client.finalized) != 1 || client.finalized[0] != "the whole answer" {
		t.Fatalf("finalized = %+v", client.finalized)
	}
	if len(client.deleted) != 0 {
		t.Fatal("a delivered answer was deleted")
	}
}

// Telegram being briefly unavailable costs the typing effect, not the turn.
func TestPreviewSurvivesAFailedCreate(t *testing.T) {
	client := &fakePreviewClient{sendErr: errors.New("429 too many requests")}
	p := newTestPreview(client)

	p.appendText(context.Background(), "text nobody can preview")
	p.settle(context.Background(), true)

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

	p.appendText(context.Background(), "text that would otherwise post")
	p.noteTool(context.Background(), "browser_fetch")
	p.clearTool()
	p.settle(context.Background(), true)

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
