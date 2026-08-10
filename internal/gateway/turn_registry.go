package gateway

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

// turnHandle tracks one in-flight turn so a later, unrelated request can
// stop it. Two things live here and both are load-bearing:
//
//   - cancel is the only way to stop a turn. The transports deliberately
//     decouple the turn's work context from the request context (a browser
//     refresh must not kill a generation half-way), so nothing about the
//     inbound connection can end a turn any more. An explicit handle is
//     what puts that back under the user's control.
//   - partial accumulates the text already streamed out. A cancelled turn
//     must still leave an assistant message behind: a user message with no
//     reply breaks the next provider call, and the user has already read
//     the half of the answer that arrived.
type turnHandle struct {
	id        string
	cancel    context.CancelFunc
	startedAt time.Time

	mu      sync.Mutex
	partial strings.Builder
}

func (h *turnHandle) noteText(delta string) {
	h.mu.Lock()
	h.partial.WriteString(delta)
	h.mu.Unlock()
}

func (h *turnHandle) partialText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.partial.String()
}

// captureText chains the handle's accumulator onto a stream callback set so
// the partial answer is known at cancellation time. A nil cb stays nil: the
// transports that pass one are exactly the streaming ones, and a transport
// with no deltas has no partial text to save either — its cancelled turn
// falls back to the bare interrupt marker.
func (h *turnHandle) captureText(cb *bs.StreamCallbacks) *bs.StreamCallbacks {
	if cb == nil {
		return nil
	}
	chained := *cb
	inner := cb.OnText
	chained.OnText = func(delta string) {
		h.noteText(delta)
		if inner != nil {
			inner(delta)
		}
	}
	return &chained
}

// TurnStatus is the public view of an in-flight turn: enough for a client
// to render "still writing" and to address a stop at this turn and not at
// whatever runs next.
type TurnStatus struct {
	ID        string
	StartedAt time.Time
}

// beginTurn registers a cancellable turn for one conversation and returns
// the context the turn must run under, its handle, and a release func the
// caller defers. Registration is keyed by conversation, which is the same
// key that serialises turns — so there is at most one entry per key and no
// ambiguity about which turn a stop refers to.
func (g *Gateway) beginTurn(ctx context.Context, userID, soulID uuid.UUID) (context.Context, *turnHandle, func()) {
	turnCtx, cancel := context.WithCancel(ctx)
	h := &turnHandle{
		id:        uuid.NewString(),
		cancel:    cancel,
		startedAt: time.Now(),
	}
	key := conversationKey(userID, soulID)
	g.activeTurns.Store(key, h)
	return turnCtx, h, func() {
		g.activeTurns.CompareAndDelete(key, h)
		cancel()
	}
}

// CancelTurn stops the turn in flight for one conversation and reports
// whether anything was actually cancelled.
//
// turnID is the guard against a stop that lands a moment too late: the user
// taps stop as the turn ends and the next turn has already started, and an
// unqualified cancel would kill an answer nobody asked to stop. An empty
// turnID means "whatever is running" and is for operator-facing paths (a
// /stop command typed with no idea of ids), not for UI buttons.
func (g *Gateway) CancelTurn(userID, soulID uuid.UUID, turnID string) bool {
	v, ok := g.activeTurns.Load(conversationKey(userID, soulID))
	if !ok {
		return false
	}
	h, ok := v.(*turnHandle)
	if !ok {
		return false
	}
	if turnID != "" && turnID != h.id {
		return false
	}
	h.cancel()
	return true
}

// ActiveTurn reports the turn currently running for a conversation, if any.
// A client that reconnects mid-answer (page reload) reads it to restore the
// live state — including a working stop button — instead of guessing from
// the shape of the history.
func (g *Gateway) ActiveTurn(userID, soulID uuid.UUID) (TurnStatus, bool) {
	v, ok := g.activeTurns.Load(conversationKey(userID, soulID))
	if !ok {
		return TurnStatus{}, false
	}
	h, ok := v.(*turnHandle)
	if !ok {
		return TurnStatus{}, false
	}
	return TurnStatus{ID: h.id, StartedAt: h.startedAt}, true
}
