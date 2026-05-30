package ws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/gateway"
)

// connWriter serialises writes to a single WebSocket connection. The barge-in
// path has multiple writers (the turn goroutine's sink and the turn manager's
// duck/resume/canceled signals); nhooyr.io/websocket allows only one writer at
// a time, so every write goes through this mutex.
type connWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func newConnWriter(conn *websocket.Conn) *connWriter {
	return &connWriter{conn: conn}
}

func (w *connWriter) write(ctx context.Context, msg OutMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.Write(ctx, websocket.MessageText, data)
}

// turnHandle tracks one in-flight turn: its cancel func and a running buffer
// of the text spoken so far (fed by the sink's NoteSpokenText), which the
// interjection classifier reads to know what the assistant is currently saying.
type turnHandle struct {
	cancel    context.CancelFunc
	startedAt time.Time

	mu     sync.Mutex
	spoken strings.Builder
}

func (h *turnHandle) noteSpoken(s string) {
	h.mu.Lock()
	h.spoken.WriteString(s)
	h.mu.Unlock()
}

func (h *turnHandle) spokenText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.spoken.String()
}

// interjectResult carries a finished interjection classification back to the
// turn manager's run loop.
type interjectResult struct {
	handle *turnHandle
	class  bs.InterjectionClass
	text   string
}

// turnManager runs the barge-in voice loop as a single-owner actor: the run
// loop owns all mutable state (active turn, pending turn) and is the only
// goroutine that touches it. Slow work — the turn itself, transcription and
// interjection classification — runs in goroutines that report back over
// channels. No locks on turn-manager state; no lock-ordering hazards.
type turnManager struct {
	gw     *gateway.Gateway
	auth   connAuth
	w      *connWriter
	logger *slog.Logger

	active  *turnHandle        // the turn currently streaming, nil when idle
	pending *bs.InboundMessage // a turn queued to start once active ends

	turnDone  chan *turnHandle
	interject chan interjectResult
}

func newTurnManager(gw *gateway.Gateway, auth connAuth, w *connWriter, logger *slog.Logger) *turnManager {
	return &turnManager{
		gw:        gw,
		auth:      auth,
		w:         w,
		logger:    logger,
		turnDone:  make(chan *turnHandle, 4),
		interject: make(chan interjectResult, 4),
	}
}

// run is the actor loop. It returns when the connection context is done or the
// inbound channel closes (read goroutine exited).
func (tm *turnManager) run(connCtx context.Context, inbound <-chan InMsg) {
	for {
		select {
		case <-connCtx.Done():
			tm.cancelActive()
			return
		case h := <-tm.turnDone:
			tm.onTurnDone(connCtx, h)
		case ir := <-tm.interject:
			tm.onInterjection(connCtx, ir)
		case msg, ok := <-inbound:
			if !ok {
				tm.cancelActive()
				return
			}
			tm.dispatch(connCtx, msg)
		}
	}
}

// dispatch routes one inbound frame.
func (tm *turnManager) dispatch(connCtx context.Context, msg InMsg) {
	switch msg.Type {
	case "speech_start":
		// The user started talking over the response — tell the client to
		// duck playback while we wait for the transcript + classification.
		if tm.active != nil {
			tm.w.write(connCtx, OutMsg{Type: "duck"})
		}
	case "cancel":
		// Explicit stop (the user pressed a stop control).
		if tm.active != nil {
			tm.logger.Info("ws: explicit cancel")
			tm.cancelActive()
			tm.w.write(connCtx, OutMsg{Type: "canceled"})
		}
	case "text", "audio":
		inb, err := toInbound(msg)
		if err != nil {
			tm.w.write(connCtx, OutMsg{Type: "error", Data: err.Error()})
			return
		}
		if tm.active == nil {
			tm.startTurn(connCtx, inb)
		} else {
			// A turn is in flight — this utterance is an interjection.
			tm.classifyAsync(connCtx, tm.active, inb)
		}
	default:
		tm.w.write(connCtx, OutMsg{Type: "error", Data: "unknown message type"})
	}
}

// classifyAsync transcribes (if needed) and classifies an interjection that
// arrived while a turn is in flight, reporting the verdict to the run loop.
func (tm *turnManager) classifyAsync(connCtx context.Context, h *turnHandle, inb bs.InboundMessage) {
	go func() {
		text := strings.TrimSpace(inb.Text)
		if len(inb.Audio) > 0 {
			t, err := tm.gw.TranscribeAudio(connCtx, inb.Audio)
			if err != nil {
				tm.logger.Warn("ws: interjection transcribe failed", "error", err)
				return
			}
			text = strings.TrimSpace(t)
		}
		if text == "" {
			return
		}
		class, err := tm.gw.ClassifyInterjection(connCtx, text, h.spokenText())
		if err != nil {
			tm.logger.Warn("ws: classify interjection failed", "error", err)
			class = bs.InterjectionUnclear
		}
		select {
		case tm.interject <- interjectResult{handle: h, class: class, text: text}:
		case <-connCtx.Done():
		}
	}()
}

// onInterjection applies a classification verdict.
func (tm *turnManager) onInterjection(connCtx context.Context, ir interjectResult) {
	if tm.active != ir.handle {
		// The turn being classified already ended — treat the utterance as
		// a fresh turn.
		tm.startOrQueue(connCtx, bs.InboundMessage{Text: ir.text})
		return
	}
	switch ir.class {
	case bs.InterjectionInterrupt:
		tm.logger.Info("ws: interjection = interruption, cancelling turn")
		tm.cancelActive()
		tm.w.write(connCtx, OutMsg{Type: "canceled"})
		// Queue the utterance — onTurnDone starts it once the cancelled
		// turn has fully unwound (and persisted its partial response).
		tm.pending = &bs.InboundMessage{Text: ir.text}
	default:
		// backchannel / unclear — keep the turn running, un-duck the client.
		tm.logger.Info("ws: interjection = backchannel, continuing", "class", string(ir.class))
		tm.w.write(connCtx, OutMsg{Type: "resume"})
	}
}

// onTurnDone clears the finished turn and starts any queued one.
func (tm *turnManager) onTurnDone(connCtx context.Context, h *turnHandle) {
	if tm.active == h {
		tm.active = nil
	}
	if tm.active == nil && tm.pending != nil {
		p := *tm.pending
		tm.pending = nil
		tm.startTurn(connCtx, p)
	}
}

// startOrQueue starts a turn now if idle, otherwise queues it.
func (tm *turnManager) startOrQueue(connCtx context.Context, inb bs.InboundMessage) {
	if tm.active == nil {
		tm.startTurn(connCtx, inb)
		return
	}
	tm.pending = &inb
}

// startTurn launches a turn in its own goroutine with a cancellable context.
func (tm *turnManager) startTurn(connCtx context.Context, inb bs.InboundMessage) {
	turnCtx, cancel := context.WithCancel(connCtx)
	h := &turnHandle{cancel: cancel, startedAt: time.Now()}
	tm.active = h

	sink := &wsSink{logger: tm.logger, writer: tm.w, turn: h}

	go func() {
		var err error
		if tm.auth.legacy {
			err = tm.gw.ProcessInbound(turnCtx, tm.auth.chatID, []bs.InboundMessage{inb}, sink)
		} else {
			err = tm.gw.ProcessInboundForUser(turnCtx, tm.auth.userID, tm.auth.soulID, deviceTransport, []bs.InboundMessage{inb}, sink)
		}
		if err != nil && turnCtx.Err() == nil {
			tm.logger.Warn("ws: process error", "error", err)
			tm.w.write(connCtx, OutMsg{Type: "error", Data: err.Error()})
		}
		// If the turn was cancelled mid-stream, persist whatever was spoken
		// so the session keeps user/assistant alternation intact (a dangling
		// user message with no reply breaks the next turn's API call).
		if turnCtx.Err() != nil {
			if tm.auth.legacy {
				tm.gw.PersistInterrupted(connCtx, tm.auth.chatID, h.spokenText())
			} else {
				tm.gw.PersistInterruptedForUser(connCtx, tm.auth.userID, tm.auth.soulID, deviceTransport, h.spokenText())
			}
		}
		select {
		case tm.turnDone <- h:
		case <-connCtx.Done():
		}
	}()
}

// cancelActive cancels the in-flight turn, if any. It does not clear
// tm.active — onTurnDone does that once the turn goroutine has unwound, so
// the partial-response persist happens before any queued turn starts.
func (tm *turnManager) cancelActive() {
	if tm.active != nil {
		tm.active.cancel()
	}
}

// toInbound converts a wire frame into a transport-agnostic InboundMessage.
func toInbound(msg InMsg) (bs.InboundMessage, error) {
	var inb bs.InboundMessage
	switch msg.Type {
	case "text":
		inb.Text = msg.Data
	case "audio":
		audio, err := base64.StdEncoding.DecodeString(msg.Data)
		if err != nil {
			return inb, fmt.Errorf("invalid base64 audio")
		}
		inb.Audio = audio
	default:
		return inb, fmt.Errorf("unknown message type")
	}
	return inb, nil
}
