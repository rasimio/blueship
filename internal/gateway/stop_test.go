package gateway

import (
	"context"
	"testing"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

func TestCancelTurnStopsTheTurnItNames(t *testing.T) {
	g := &Gateway{}
	userID, soulID := uuid.New(), uuid.New()

	ctx, turn, end := g.beginTurn(context.Background(), userID, soulID)
	defer end()

	if !g.CancelTurn(userID, soulID, turn.id) {
		t.Fatal("CancelTurn reported nothing to stop while a turn was registered")
	}
	if ctx.Err() == nil {
		t.Fatal("turn context still live after CancelTurn")
	}
}

// The window this guards is small and real: a person taps stop as the answer
// lands, and by the time the tap arrives the next turn has started. Cancelling
// "whatever is running" there kills an answer nobody asked to stop.
func TestCancelTurnIgnoresAStaleTurnID(t *testing.T) {
	g := &Gateway{}
	userID, soulID := uuid.New(), uuid.New()

	ctx, _, end := g.beginTurn(context.Background(), userID, soulID)
	defer end()

	if g.CancelTurn(userID, soulID, uuid.NewString()) {
		t.Fatal("CancelTurn accepted an id that names a different turn")
	}
	if ctx.Err() != nil {
		t.Fatal("a stale stop cancelled the running turn")
	}
}

// An empty id is the typed /stop command, which has no way to know one.
func TestCancelTurnWithoutIDStopsWhateverIsRunning(t *testing.T) {
	g := &Gateway{}
	userID, soulID := uuid.New(), uuid.New()

	ctx, _, end := g.beginTurn(context.Background(), userID, soulID)
	defer end()

	if !g.CancelTurn(userID, soulID, "") {
		t.Fatal("unqualified CancelTurn found nothing to stop")
	}
	if ctx.Err() == nil {
		t.Fatal("turn context still live after unqualified CancelTurn")
	}
}

func TestCancelTurnOnIdleConversationReportsNothingStopped(t *testing.T) {
	g := &Gateway{}
	userID, soulID := uuid.New(), uuid.New()

	if g.CancelTurn(userID, soulID, "") {
		t.Fatal("CancelTurn claimed to stop a turn on an idle conversation")
	}

	_, _, end := g.beginTurn(context.Background(), userID, soulID)
	end()

	if g.CancelTurn(userID, soulID, "") {
		t.Fatal("a finished turn is still registered")
	}
	if _, running := g.ActiveTurn(userID, soulID); running {
		t.Fatal("ActiveTurn reports a turn that has ended")
	}
}

// One conversation's stop must not reach another's.
func TestCancelTurnIsScopedToOneConversation(t *testing.T) {
	g := &Gateway{}
	userID, soulID := uuid.New(), uuid.New()

	ctx, _, end := g.beginTurn(context.Background(), userID, soulID)
	defer end()

	if g.CancelTurn(uuid.New(), soulID, "") {
		t.Fatal("stop crossed into another user's conversation")
	}
	if g.CancelTurn(userID, uuid.New(), "") {
		t.Fatal("stop crossed into another soul's conversation")
	}
	if ctx.Err() != nil {
		t.Fatal("a foreign stop cancelled this turn")
	}
}

func TestActiveTurnNamesTheRunningTurn(t *testing.T) {
	g := &Gateway{}
	userID, soulID := uuid.New(), uuid.New()

	if _, running := g.ActiveTurn(userID, soulID); running {
		t.Fatal("idle conversation reported as streaming")
	}

	_, turn, end := g.beginTurn(context.Background(), userID, soulID)
	defer end()

	status, running := g.ActiveTurn(userID, soulID)
	if !running {
		t.Fatal("running turn not reported")
	}
	if status.ID != turn.id {
		t.Fatalf("ActiveTurn id = %q, want %q", status.ID, turn.id)
	}
	if status.StartedAt.IsZero() {
		t.Fatal("ActiveTurn has no start time; a reconnecting client cannot render one")
	}
}

// The partial answer is the whole point of capturing text: a cancelled turn
// still has to leave behind what the user already read.
func TestCaptureTextAccumulatesWithoutSwallowingTheDelta(t *testing.T) {
	h := &turnHandle{}
	var delivered string
	cb := h.captureText(&bs.StreamCallbacks{
		OnText: func(delta string) { delivered += delta },
	})

	cb.OnText("half an ")
	cb.OnText("answer")

	if got := h.partialText(); got != "half an answer" {
		t.Fatalf("partial = %q, want %q", got, "half an answer")
	}
	if delivered != "half an answer" {
		t.Fatalf("inner callback got %q — capture swallowed the stream", delivered)
	}
}

// Transports with nothing to stream (no deltas) pass no callbacks, and
// wrapping nil into a non-nil set would hand the agent loop a callback set it
// was never given.
func TestCaptureTextLeavesAbsentCallbacksAbsent(t *testing.T) {
	h := &turnHandle{}
	if cb := h.captureText(nil); cb != nil {
		t.Fatalf("captureText(nil) = %+v, want nil", cb)
	}
}

func TestCaptureTextSurvivesACallbackSetWithNoOnText(t *testing.T) {
	h := &turnHandle{}
	cb := h.captureText(&bs.StreamCallbacks{})
	if cb == nil || cb.OnText == nil {
		t.Fatal("captureText dropped the accumulator for a set with no OnText")
	}
	cb.OnText("only capture listens")
	if got := h.partialText(); got != "only capture listens" {
		t.Fatalf("partial = %q", got)
	}
}

func TestStopKeyboardCarriesTheTurnIDWithinTelegramsCallbackLimit(t *testing.T) {
	turnID := uuid.NewString()
	rows := stopKeyboard(turnID, "⏹ Stop")

	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("stop keyboard shape = %+v, want a single button", rows)
	}
	button := rows[0][0]
	if button.CallbackData != stopCallbackPrefix+turnID {
		t.Fatalf("callback data = %q", button.CallbackData)
	}
	if button.URL != "" {
		t.Fatalf("a callback button carrying a url is rejected for the whole keyboard: %+v", button)
	}
	// Telegram rejects callback_data over 64 bytes — silently, for the whole
	// keyboard, which would leave the reply with no stop control at all.
	if n := len(button.CallbackData); n > 64 {
		t.Fatalf("callback data is %d bytes, over Telegram's 64-byte limit", n)
	}
}

// Every inline keyboard in the product lands on the same callback handler,
// and stop answers the query itself — so claiming one that isn't a stop would
// eat another feature's button press.
func TestMaybeHandleStopCallbackIgnoresOtherKeyboards(t *testing.T) {
	g := &Gateway{}
	if g.maybeHandleStopCallback(context.Background(), nil, nil) {
		t.Fatal("nil callback claimed as a stop")
	}
	other := &telegram.CallbackQuery{ID: "1", Data: "onboarding:seed:2"}
	if g.maybeHandleStopCallback(context.Background(), nil, other) {
		t.Fatal("an onboarding callback was claimed as a stop")
	}
}
