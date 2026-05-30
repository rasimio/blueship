// Package server is blueship's A2A HTTP server. It hosts
//
//	GET  /.well-known/agent   — agent card (name, endpoints, exposed tools)
//	POST /a2a/invoke          — invoke an exposed tool (sync or async)
//	GET  /a2a/events          — Server-Sent Events stream for async calls
//	GET  /a2a/health          — liveness
//
// Handlers delegate to a2a.store for persistence and to a domain-owned
// Dispatcher (passed in at construction) for actually running exposed tools.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rasimio/blueship/internal/federation/a2a"
	"github.com/rasimio/blueship/internal/federation/a2a/store"
)

// Dispatcher resolves an incoming tool invocation to a locally-registered
// handler and runs it. The server does not care whether the underlying
// module uses goroutines, channels or subprocesses — it only needs an
// implementation that returns output synchronously (sync mode) or streams
// events via the EventEmitter argument (async mode).
type Dispatcher interface {
	Tool(name string) (a2a.ExposedTool, bool)
	ExposedTools() []a2a.ExposedTool
	InvokeSync(ctx context.Context, name string, input json.RawMessage) (json.RawMessage, error)
	InvokeAsync(ctx context.Context, name string, input json.RawMessage, emit a2a.EventEmitter) (initial json.RawMessage, err error)
}

// JWTValidator validates an incoming bearer JWT against this Ship's
// expected audience (its own Fleet agent_id). Returns the caller's agent
// id (sub claim) on success. Set on Server.Config to enable Fleet-issued
// JWT auth alongside the legacy shared bearer.
type JWTValidator func(ctx context.Context, raw string) (callerAgentID string, err error)

// Config holds server startup parameters.
type Config struct {
	Name        string
	Description string
	Version     string
	BaseURL     string
	AuthToken   string       // shared secret; empty disables auth (dev only)
	JWTValidator JWTValidator // optional; when set, JWT auth runs before AuthToken fallback
}

// Server is the A2A HTTP server.
type Server struct {
	cfg             Config
	store           *store.Store
	disp            Dispatcher
	callbackHandler a2a.CallbackHandler
	logger          *slog.Logger
	mux             *http.ServeMux

	// live SSE subscribers: map[callID] -> list of channels. Each running
	// async call gets one channel per connected SSE client; Emit writes
	// to both the DB and every live channel so new events push instantly.
	mu   sync.Mutex
	subs map[string][]chan a2a.Event
}

// New constructs a Server. Call Routes() (or RegisterOn) to install its
// handlers on an http.ServeMux owned by the caller.
func New(cfg Config, st *store.Store, disp Dispatcher, cbHandler a2a.CallbackHandler, logger *slog.Logger) *Server {
	s := &Server{
		cfg:             cfg,
		store:           st,
		disp:            disp,
		callbackHandler: cbHandler,
		logger:          logger,
		mux:             http.NewServeMux(),
		subs:            make(map[string][]chan a2a.Event),
	}
	s.routes()
	return s
}

// Handler exposes the server's mux so it can be plugged into an existing
// http.Server or composed with other routes.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// routes wires the HTTP endpoints.
func (s *Server) routes() {
	s.mux.HandleFunc("/.well-known/agent", s.handleAgentCard)
	s.mux.HandleFunc("/a2a/invoke", s.auth(s.handleInvoke))
	s.mux.HandleFunc("/a2a/events", s.auth(s.handleEvents))
	s.mux.HandleFunc("/a2a/callback", s.auth(s.handleCallback))
	s.mux.HandleFunc("/a2a/health", s.handleHealth)
}

// auth wraps a handler in bearer-token check. Two acceptable shapes:
//
//  1. Fleet-issued JWT: validated via cfg.JWTValidator. The caller's
//     agent_id (sub claim) is stamped onto X-A2A-Peer for downstream
//     audit, replacing whatever the client sent.
//  2. Legacy shared secret matching cfg.AuthToken. Kept during the
//     Phase 8 → Phase 9 cutover so any peer that has not yet migrated
//     to Fleet still works.
//
// If neither is configured, requests pass through (single-owner dev mode).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthToken == "" && s.cfg.JWTValidator == nil {
			next(w, r)
			return
		}
		raw := r.Header.Get("Authorization")
		bearer, ok := strings.CutPrefix(raw, "Bearer ")
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token", "")
			return
		}
		bearer = strings.TrimSpace(bearer)
		// Try JWT first if configured.
		if s.cfg.JWTValidator != nil {
			caller, err := s.cfg.JWTValidator(r.Context(), bearer)
			if err == nil {
				r.Header.Set("X-A2A-Peer", caller)
				next(w, r)
				return
			}
			s.logger.Debug("a2a: jwt rejected, trying static bearer", "error", err)
		}
		// Fallback to legacy shared secret.
		if s.cfg.AuthToken != "" && bearer == s.cfg.AuthToken {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token", "")
	}
}

// handleAgentCard returns /.well-known/agent — used by peers on startup
// to discover available tools.
func (s *Server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	tools := s.disp.ExposedTools()
	card := a2a.AgentCard{
		Name:        s.cfg.Name,
		Description: s.cfg.Description,
		Version:     s.cfg.Version,
		BaseURL:     s.cfg.BaseURL,
		Endpoints: a2a.Endpoints{
			Invoke: "/a2a/invoke",
			Events: "/a2a/events",
			Cancel: "/a2a/cancel",
		},
		Auth:  a2a.AuthInfo{Type: "bearer"},
		Tools: tools,
	}
	writeJSON(w, http.StatusOK, card)
}

// handleHealth is a cheap liveness endpoint.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleCallback is POST /a2a/callback — fire-and-forget push notification.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10)) // 64KB
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", err.Error(), "")
		return
	}
	var cb a2a.Callback
	if err := json.Unmarshal(body, &cb); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error(), "")
		return
	}
	s.logger.Info("a2a: callback received", "peer", cb.Peer, "event", cb.Event)
	if s.callbackHandler != nil {
		go s.callbackHandler(r.Context(), cb)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleInvoke is POST /a2a/invoke.
func (s *Server) handleInvoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20)) // 4MB guard
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", err.Error(), "")
		return
	}
	var req a2a.InvokeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error(), "")
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, "missing_tool", "tool required", "")
		return
	}

	tool, ok := s.disp.Tool(req.Tool)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_tool", req.Tool, "")
		return
	}

	peerName := r.Header.Get("X-A2A-Peer")
	if peerName == "" {
		peerName = "unknown"
	}
	corr := req.CorrelationID
	var corrPtr *string
	if corr != "" {
		corrPtr = &corr
	}

	callRec := a2a.Call{
		PeerName:      peerName,
		Direction:     a2a.CallDirectionIn,
		ToolName:      req.Tool,
		Mode:          tool.Mode,
		CorrelationID: corrPtr,
		Input:         req.Input,
		State:         a2a.CallStatePending,
	}
	callID, err := s.store.CreateCall(r.Context(), callRec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error(), "")
		return
	}

	switch tool.Mode {
	case a2a.ToolModeSync:
		s.runSync(w, r, callID, tool, req.Input)
	case a2a.ToolModeAsync:
		s.runAsync(w, r, callID, tool, req.Input)
	default:
		writeError(w, http.StatusInternalServerError, "bad_mode", string(tool.Mode), "")
	}
}

// runSync executes a sync tool and returns the output inline.
func (s *Server) runSync(w http.ResponseWriter, r *http.Request, callID string, tool a2a.ExposedTool, input json.RawMessage) {
	_ = s.store.UpdateCallState(r.Context(), callID, a2a.CallStateRunning, nil, "")
	out, runErr := s.disp.InvokeSync(r.Context(), tool.Name, input)
	if runErr != nil {
		_ = s.store.UpdateCallState(r.Context(), callID, a2a.CallStateFailed, nil, runErr.Error())
		writeError(w, http.StatusInternalServerError, "tool_error", runErr.Error(), callID)
		return
	}
	_ = s.store.UpdateCallState(r.Context(), callID, a2a.CallStateDone, out, "")
	writeJSON(w, http.StatusOK, a2a.InvokeResponse{
		CallID: callID,
		Mode:   a2a.ToolModeSync,
		State:  a2a.CallStateDone,
		Output: out,
	})
}

// runAsync executes an async tool: it returns immediately with a handle,
// then continues emitting events from a background goroutine.
func (s *Server) runAsync(w http.ResponseWriter, r *http.Request, callID string, tool a2a.ExposedTool, input json.RawMessage) {
	_ = s.store.UpdateCallState(r.Context(), callID, a2a.CallStateRunning, nil, "")

	emitter := &eventEmitter{
		s:      s,
		callID: callID,
		logger: s.logger,
	}

	// Detach from the request context so the background work survives
	// the HTTP response being written.
	bgCtx := context.Background()

	initial, err := s.disp.InvokeAsync(bgCtx, tool.Name, input, emitter)
	if err != nil {
		_ = emitter.EmitTerminal(bgCtx, a2a.CallStateFailed, marshalErr(err))
		writeError(w, http.StatusInternalServerError, "tool_error", err.Error(), callID)
		return
	}

	writeJSON(w, http.StatusAccepted, a2a.InvokeResponse{
		CallID:    callID,
		Mode:      a2a.ToolModeAsync,
		State:     a2a.CallStateRunning,
		Output:    initial,
		Handle:    callID,
		EventsURL: fmt.Sprintf("/a2a/events?call=%s&since=0", callID),
	})
}

// handleEvents serves /a2a/events via Server-Sent Events.
//
//	GET /a2a/events?call=<uuid>&since=<int>
//
// First streams any buffered events with seq > since, then subscribes to
// the in-memory channel and pushes live events until the caller disconnects
// or a terminal event is emitted.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	callID := r.URL.Query().Get("call")
	if callID == "" {
		writeError(w, http.StatusBadRequest, "missing_call", "call query param required", "")
		return
	}
	sinceStr := r.URL.Query().Get("since")
	since, _ := strconv.Atoi(sinceStr)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_flush", "sse requires http.Flusher", "")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Replay persisted events first so reconnects don't lose anything.
	replay, err := s.store.EventsSince(r.Context(), callID, since, 500)
	if err != nil {
		s.writeSSE(w, "error", marshalErr(err))
		flusher.Flush()
		return
	}
	var lastSeq int
	var sawFinal bool
	for _, ev := range replay {
		s.writeSSEEvent(w, ev)
		flusher.Flush()
		lastSeq = ev.Seq
		if ev.IsFinal {
			sawFinal = true
		}
	}
	if sawFinal {
		return
	}

	// Check again to avoid the race between replay-scan and live-subscribe.
	if terminal, _ := s.store.HasTerminalEvent(r.Context(), callID); terminal {
		// Final event landed during replay — fetch tail and return.
		tail, _ := s.store.EventsSince(r.Context(), callID, lastSeq, 100)
		for _, ev := range tail {
			s.writeSSEEvent(w, ev)
			flusher.Flush()
		}
		return
	}

	ch := s.subscribe(callID)
	defer s.unsubscribe(callID, ch)

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			if ev.Seq <= lastSeq {
				continue
			}
			s.writeSSEEvent(w, ev)
			flusher.Flush()
			lastSeq = ev.Seq
			if ev.IsFinal {
				return
			}
		case <-heartbeat.C:
			// Comment lines keep proxies from closing idle connections.
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// subscribe registers a fresh channel for a call.
func (s *Server) subscribe(callID string) chan a2a.Event {
	ch := make(chan a2a.Event, 16)
	s.mu.Lock()
	s.subs[callID] = append(s.subs[callID], ch)
	s.mu.Unlock()
	return ch
}

// unsubscribe removes and closes a channel.
func (s *Server) unsubscribe(callID string, ch chan a2a.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.subs[callID]
	out := list[:0]
	for _, c := range list {
		if c != ch {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		delete(s.subs, callID)
	} else {
		s.subs[callID] = out
	}
	close(ch)
}

// fanoutEvent pushes an event to every live subscriber for this call.
// Called by the eventEmitter right after the row is persisted.
func (s *Server) fanoutEvent(callID string, ev a2a.Event) {
	s.mu.Lock()
	list := append([]chan a2a.Event(nil), s.subs[callID]...)
	s.mu.Unlock()
	for _, ch := range list {
		select {
		case ch <- ev:
		default:
			s.logger.Warn("a2a: sse subscriber dropped event", "call_id", callID, "seq", ev.Seq)
		}
	}
}

// writeSSEEvent serialises an Event as an SSE data block.
func (s *Server) writeSSEEvent(w http.ResponseWriter, ev a2a.Event) {
	body, err := json.Marshal(ev)
	if err != nil {
		s.logger.Warn("a2a: marshal event failed", "error", err)
		return
	}
	fmt.Fprintf(w, "id: %d\n", ev.Seq)
	fmt.Fprintf(w, "event: %s\n", ev.Type)
	fmt.Fprintf(w, "data: %s\n\n", body)
}

// writeSSE is a simple one-off SSE message (used for errors before the
// main loop).
func (s *Server) writeSSE(w http.ResponseWriter, event string, payload json.RawMessage) {
	fmt.Fprintf(w, "event: %s\n", event)
	fmt.Fprintf(w, "data: %s\n\n", payload)
}

// eventEmitter is the EventEmitter implementation handed to async handlers.
// It persists every event via the store and also pushes it to live SSE
// subscribers through the server's fanout hub.
type eventEmitter struct {
	s      *Server
	callID string
	logger *slog.Logger
}

func (e *eventEmitter) Emit(ctx context.Context, eventType a2a.EventType, payload json.RawMessage) error {
	ev, err := e.s.store.AppendEvent(ctx, e.callID, eventType, payload, false)
	if err != nil {
		return err
	}
	e.s.fanoutEvent(e.callID, *ev)
	return nil
}

func (e *eventEmitter) EmitTerminal(ctx context.Context, finalState a2a.CallState, payload json.RawMessage) error {
	if !finalState.IsTerminal() {
		return fmt.Errorf("EmitTerminal: non-terminal state %q", finalState)
	}
	ev, err := e.s.store.AppendEvent(ctx, e.callID, a2a.EventTypeTerminal, payload, true)
	if err != nil {
		return err
	}
	errText := ""
	if finalState == a2a.CallStateFailed {
		errText = string(payload)
	}
	_ = e.s.store.UpdateCallState(ctx, e.callID, finalState, payload, errText)
	e.s.fanoutEvent(e.callID, *ev)
	return nil
}

// ---------------------------------------------------------------------------
// tiny HTTP helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, msg, details string) {
	writeJSON(w, status, map[string]any{
		"error": a2a.APIError{Code: code, Message: msg, Details: details},
	})
}

func marshalErr(err error) json.RawMessage {
	if err == nil {
		return json.RawMessage(`{}`)
	}
	var apiErr a2a.APIError
	if errors.As(err, &apiErr) {
		b, _ := json.Marshal(apiErr)
		return b
	}
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	return b
}
