// Package httpchat is an HTTP transport that streams a soul's chat
// response as Server-Sent Events. It serves the Vaelum web platform: the
// vaelum backend relays an authenticated user's message here, and the SSE
// stream is piped straight back to the browser.
//
// The same server also hosts host-supplied internal-API routes via the
// Extras callback on HTTPChatConfig — the host plugs its associate
// endpoint in this way. All routes share the bearer-token middleware so
// the host's extras are authed without each handler re-implementing it.
package httpchat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/attachment"
	pdfint "github.com/rasimio/blueship/integration/pdf"
	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/gateway"
	"github.com/rasimio/blueship/internal/webaccess/browser"
)

// Server is the HTTP/SSE chat transport.
type Server struct {
	gw               *gateway.Gateway
	port             int
	token            string
	transportName    string // source tag on inbound messages (e.g. the platform name)
	validateUserSoul func(ctx context.Context, userID, soulID uuid.UUID) error
	extras           func(*http.ServeMux)
	reset            func(ctx context.Context, userID string) (string, string, error)
	invokeTool       func(context.Context, uuid.UUID, uuid.UUID, string, json.RawMessage) (gateway.ToolInvocation, error)
	refreshMCP       func(context.Context, uuid.UUID, uuid.UUID) (int, error)
	logger           *slog.Logger
}

// NewServer creates an HTTP chat server attached to an existing Gateway.
// token is the shared service token vaelum must present; empty disables
// auth. extras, when non-nil, is called once during Run with the server's
// mux so the host can mount additional routes (they share the bearer
// middleware). reset, when non-nil, exposes POST /api/internal/chat/reset
// — vaelum's web cabinet calls it to archive the active session and
// open a fresh one (equivalent of the Telegram /reset command).
// transportName is the source tag stamped on inbound messages (used for
// session source / observability); empty defaults to "http".
func NewServer(gw *gateway.Gateway, port int, token, transportName string, validateUserSoul func(context.Context, uuid.UUID, uuid.UUID) error, extras func(*http.ServeMux), reset func(context.Context, string) (string, string, error), logger *slog.Logger) *Server {
	if transportName == "" {
		transportName = "http"
	}
	return &Server{
		gw: gw, port: port, token: token, transportName: transportName,
		validateUserSoul: validateUserSoul, extras: extras, reset: reset,
		invokeTool: gw.InvokeToolForUser, refreshMCP: gw.RefreshMCPForUser,
		logger: logger,
	}
}

// Run starts the HTTP server. Blocks until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	handler := s.handler()

	addr := fmt.Sprintf(":%d", s.port)
	srv := &http.Server{Addr: addr, Handler: handler}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	s.logger.Info("http chat server started", "addr", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("httpchat server: %w", err)
	}
	return nil
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat", s.handleChat)
	mux.HandleFunc("POST /chat/stop", s.handleStop)
	mux.HandleFunc("GET /chat/state", s.handleState)
	mux.Handle(
		"POST /api/internal/tools/invoke",
		s.requireInternalBearer(http.HandlerFunc(s.handleInvokeTool)),
	)
	mux.Handle(
		"POST /api/internal/mcp/refresh",
		s.requireInternalBearer(http.HandlerFunc(s.handleRefreshMCP)),
	)
	if s.reset != nil {
		mux.Handle(
			"POST /api/internal/chat/reset",
			s.requireInternalBearer(http.HandlerFunc(s.handleReset)),
		)
	}
	if s.extras != nil {
		s.extras(mux)
	}

	handler := http.Handler(mux)
	if s.token != "" {
		handler = s.requireBearer(handler)
	}
	return handler
}

// requireBearer is the auth middleware applied to every route on the mux
// (both `/chat` and host-supplied extras). Vaelum is the only trusted
// caller; the token comes from the shared VAELUM_DAEMON_SERVICE_TOKEN env.
func (s *Server) requireBearer(next http.Handler) http.Handler {
	want := "Bearer " + s.token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireInternalBearer fails closed when the host omitted its service token
// or passed an unexpanded ${ENV_VAR} placeholder. The public /chat route keeps
// its historical empty-token development mode, but privileged internal routes
// are never exposed through that fallback.
func (s *Server) requireInternalBearer(next http.Handler) http.Handler {
	token := strings.TrimSpace(s.token)
	configured := token != "" && !strings.Contains(token, "${")
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !configured {
			http.Error(w, "internal API authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type chatRequest struct {
	UserID      string           `json:"user_id"`
	SoulID      string           `json:"soul_id"`
	Text        string           `json:"text"`
	Attachments []chatAttachment `json:"attachments,omitempty"`
	// ReplyToMessageID, when set, is the uuid of the cabinet
	// chat_messages row this turn replies to. The gateway stamps it
	// onto the new user row's reply_to_message_id column so the
	// cabinet's history endpoint can render a relational reply-
	// quote chip. Empty for non-reply turns; Telegram replies use
	// the gateway's tg_message_id index instead.
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	// Source tags the origin. "notebook" → an ephemeral, private ask:
	// the turn uses memory for context but persists nothing and never
	// appears in the chat thread. Empty = a normal chat turn.
	Source string `json:"source,omitempty"`
}

// chatAttachment is one file attached to a cabinet message. The caller
// (vaelum backend) classifies kind from the source MimeType + filename
// so the daemon can route images to the vision content path and
// text/PDF docs to the in-prompt text inline path, matching how the
// Telegram gateway handles the equivalent message shape.
type chatAttachment struct {
	// Kind is "image" | "pdf" | "text". Unknown kinds are ignored —
	// callers should pre-filter rather than dump unknown bytes at us.
	Kind string `json:"kind"`
	// MimeType for images is forwarded verbatim into the vision block;
	// for PDFs/text it's diagnostic only.
	MimeType string `json:"mime_type"`
	// Name is the original filename, surfaced in the rendered text
	// header ([file: x.go] / [pdf: y.pdf — N pages]) so the model can
	// cite the source.
	Name string `json:"name"`
	// DataB64 is the raw bytes, base64-standard-encoded. Capped server
	// side by the request body limit (we don't enforce a per-file cap
	// beyond that — vaelum already rejects oversized uploads).
	DataB64 string `json:"data_b64"`
}

// stopRequest asks for the turn in flight on one conversation to end.
// TurnID names the generation the caller means: a stop pressed as a turn
// finishes must not land on the next one. Empty means "whatever is running".
type stopRequest struct {
	UserID string `json:"user_id"`
	SoulID string `json:"soul_id"`
	TurnID string `json:"turn_id,omitempty"`
}

type stopResponse struct {
	Stopped bool `json:"stopped"`
}

// stateResponse tells a reconnecting client whether an answer is being
// written right now, and which one — so a reloaded page can restore the live
// bubble with a working stop control instead of inferring it from the shape
// of the history.
type stateResponse struct {
	Streaming bool   `json:"streaming"`
	TurnID    string `json:"turn_id,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

type resetRequest struct {
	UserID string `json:"user_id"`
	SoulID string `json:"soul_id"`
}

type resetResponse struct {
	OldSessionID string `json:"old_session_id"`
	NewSessionID string `json:"new_session_id"`
}

const (
	maxToolInvokeBodyBytes = 64 << 10
	directToolTimeout      = 30 * time.Second
)

type toolInvokeRequest struct {
	UserID string          `json:"user_id"`
	SoulID string          `json:"soul_id"`
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
}

type toolInvocationView struct {
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Output    string          `json:"output"`
	IsError   bool            `json:"is_error"`
	LatencyMS int64           `json:"latency_ms"`
}

type mcpRefreshRequest struct {
	UserID string `json:"user_id"`
	SoulID string `json:"soul_id"`
}

func (s *Server) handleInvokeTool(w http.ResponseWriter, r *http.Request) {
	if s.invokeTool == nil {
		http.Error(w, "tool invocation unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxToolInvokeBodyBytes)
	var req toolInvokeRequest
	if err := decodeOneJSON(r.Body, &req); err != nil || !jsonObject(req.Input) {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	userID, soulID, ok := s.authorizeUserSoul(w, r, req.UserID, req.SoulID)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), directToolTimeout)
	defer cancel()
	invocation, err := s.invokeTool(ctx, userID, soulID, req.Name, req.Input)
	if err != nil {
		s.writeDirectToolError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invocation": toolInvocationView{
		Name: invocation.Name, Input: invocation.Input, Output: invocation.Output,
		IsError: invocation.IsError, LatencyMS: invocation.LatencyMS,
	}})
}

func (s *Server) handleRefreshMCP(w http.ResponseWriter, r *http.Request) {
	if s.refreshMCP == nil {
		http.Error(w, "MCP refresh unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxToolInvokeBodyBytes)
	var req mcpRefreshRequest
	if err := decodeOneJSON(r.Body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	userID, soulID, ok := s.authorizeUserSoul(w, r, req.UserID, req.SoulID)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), directToolTimeout)
	defer cancel()
	count, err := s.refreshMCP(ctx, userID, soulID)
	if err != nil {
		s.writeDirectToolError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"tool_count": count})
}

func (s *Server) authorizeUserSoul(w http.ResponseWriter, r *http.Request, rawUserID, rawSoulID string) (uuid.UUID, uuid.UUID, bool) {
	userID, err := uuid.Parse(rawUserID)
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return uuid.Nil, uuid.Nil, false
	}
	soulID, err := uuid.Parse(rawSoulID)
	if err != nil {
		http.Error(w, "invalid soul_id", http.StatusBadRequest)
		return uuid.Nil, uuid.Nil, false
	}
	if s.validateUserSoul != nil {
		if err := s.validateUserSoul(r.Context(), userID, soulID); err != nil {
			s.logger.Warn("httpchat: user/soul validation failed", "user_id", userID, "soul_id", soulID, "err", err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return uuid.Nil, uuid.Nil, false
		}
	}
	return userID, soulID, true
}

func (s *Server) writeDirectToolError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bs.ErrExecutionDenied):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, gateway.ErrToolNotFound):
		http.Error(w, "tool not found", http.StatusNotFound)
	case errors.Is(err, gateway.ErrToolDisabled):
		http.Error(w, "tool disabled", http.StatusConflict)
	case errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "tool execution timed out", http.StatusGatewayTimeout)
	default:
		s.logger.Warn("httpchat: direct tool request failed", "err", err)
		http.Error(w, "tool execution failed", http.StatusBadGateway)
	}
}

func decodeOneJSON(r io.Reader, dst any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func jsonObject(raw json.RawMessage) bool {
	var value map[string]any
	return len(raw) > 0 && json.Unmarshal(raw, &value) == nil && value != nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// handleReset archives the active (user, soul) chat session and creates
// a new one in its place. Soul is pinned on ctx so session.Store's
// soul-keyed lookup hits the right thread; the underlying gateway call
// returns the old + new session IDs for confirmation to the caller.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	var req resetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return
	}
	soulID, err := uuid.Parse(req.SoulID)
	if err != nil {
		http.Error(w, "invalid soul_id", http.StatusBadRequest)
		return
	}
	if s.validateUserSoul != nil {
		if err := s.validateUserSoul(r.Context(), userID, soulID); err != nil {
			s.logger.Warn("httpchat: user/soul validation failed", "user_id", userID, "soul_id", soulID, "err", err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	ctx := bs.WithSoulID(r.Context(), soulID)
	oldID, newID, err := s.reset(ctx, userID.String())
	if err != nil {
		s.logger.Warn("httpchat: reset failed", "user_id", userID, "soul_id", soulID, "err", err)
		http.Error(w, "reset failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resetResponse{OldSessionID: oldID, NewSessionID: newID})
}

// handleStop ends the turn in flight for one conversation.
//
// It exists because /chat deliberately runs its turn on a context detached
// from the request: a closed tab must not kill a generation. Nothing about
// the streaming connection can end a turn any more, so stopping needs a
// request of its own.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	var req stopRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	userID, soulID, ok := s.conversation(w, r, req.UserID, req.SoulID)
	if !ok {
		return
	}

	stopped := s.gw.CancelTurn(userID, soulID, req.TurnID)
	s.logger.Info("httpchat: stop requested",
		"user_id", userID, "soul_id", soulID, "turn_id", req.TurnID, "stopped", stopped)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(stopResponse{Stopped: stopped})
}

// handleState reports whether a turn is being written for one conversation.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	userID, soulID, ok := s.conversation(w, r, r.URL.Query().Get("user_id"), r.URL.Query().Get("soul_id"))
	if !ok {
		return
	}

	resp := stateResponse{}
	if turn, running := s.gw.ActiveTurn(userID, soulID); running {
		resp.Streaming = true
		resp.TurnID = turn.ID
		resp.StartedAt = turn.StartedAt.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// conversation parses and authorises a (user, soul) pair, writing the error
// response itself. The pairing check is the same one /chat runs: knowing the
// service token is not the same as being allowed to touch a given soul.
func (s *Server) conversation(w http.ResponseWriter, r *http.Request, rawUser, rawSoul string) (uuid.UUID, uuid.UUID, bool) {
	userID, err := uuid.Parse(rawUser)
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return uuid.Nil, uuid.Nil, false
	}
	soulID, err := uuid.Parse(rawSoul)
	if err != nil {
		http.Error(w, "invalid soul_id", http.StatusBadRequest)
		return uuid.Nil, uuid.Nil, false
	}
	if s.validateUserSoul != nil {
		if err := s.validateUserSoul(r.Context(), userID, soulID); err != nil {
			s.logger.Warn("httpchat: user/soul validation failed",
				"user_id", userID, "soul_id", soulID, "err", err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return uuid.Nil, uuid.Nil, false
		}
	}
	return userID, soulID, true
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	userID, soulID, ok := s.conversation(w, r, req.UserID, req.SoulID)
	if !ok {
		return
	}
	if req.Text == "" && len(req.Attachments) == 0 {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}

	text := req.Text
	var images []bs.ContentBlock
	for _, att := range req.Attachments {
		data, derr := base64.StdEncoding.DecodeString(att.DataB64)
		if derr != nil {
			s.logger.Warn("httpchat: bad base64 attachment", "name", att.Name, "kind", att.Kind, "err", derr)
			continue
		}
		switch att.Kind {
		case "image":
			// Rebuild media_type from the actual bytes — vaelum may
			// forward a mistyped MIME (octet-stream from a renamed
			// upload, etc), and Anthropic's vision API rejects
			// requests where declared media_type and bytes disagree.
			media := attachment.MimeForImage(data)
			if media == "" {
				s.logger.Warn("httpchat: image kind but no signature match", "name", att.Name)
				continue
			}
			images = append(images, bs.ContentBlock{
				Type: "image",
				Source: &bs.ImageSource{
					Type:      "base64",
					MediaType: media,
					Data:      att.DataB64,
				},
			})
		case "pdf":
			pdfText, pages, perr := browser.ExtractPDFText(data)
			if perr != nil {
				s.logger.Warn("httpchat: pdf extract failed", "name", att.Name, "size", len(data), "err", perr)
				text = appendInlineFile(text, fmt.Sprintf("[pdf: %s — extraction failed: %v]", att.Name, perr))
			} else if pdfint.TextLooksScanned(pdfText, pages) {
				// Scanned PDF — same vision fallback as the Telegram
				// gateway: render leading pages, let the model read them.
				pageImgs, ierr := pdfint.PagesToImages(r.Context(), data, pdfint.DefaultScanMaxPages, pdfint.DefaultScanDPI)
				if ierr != nil || len(pageImgs) == 0 {
					s.logger.Warn("httpchat: scanned pdf render unavailable", "name", att.Name, "err", ierr)
					text = appendInlineFile(text, fmt.Sprintf("[pdf: %s — %d pages, scanned (no text layer); page rendering unavailable — ask the user for a text version]", att.Name, pages))
				} else {
					text = appendInlineFile(text, fmt.Sprintf("[pdf: %s — scanned, no text layer; first %d of %d pages attached as images — read them visually]", att.Name, len(pageImgs), pages))
					for _, img := range pageImgs {
						images = append(images, bs.ContentBlock{
							Type: "image",
							Source: &bs.ImageSource{
								Type:      "base64",
								MediaType: "image/jpeg",
								Data:      base64.StdEncoding.EncodeToString(img),
							},
						})
					}
				}
			} else {
				text = appendInlineFile(text, fmt.Sprintf("[pdf: %s — %d pages]%s", att.Name, pages, pdfText))
			}
		case "xlsx":
			xlsxMD, xerr := attachment.ExtractXlsxMarkdown(data)
			if xerr != nil {
				s.logger.Warn("httpchat: xlsx extract failed", "name", att.Name, "size", len(data), "err", xerr)
				text = appendInlineFile(text, fmt.Sprintf("[xlsx: %s — could not read this Excel file]", att.Name))
			} else {
				text = appendInlineFile(text, fmt.Sprintf("[xlsx: %s]\n%s", att.Name, xlsxMD))
			}
		case "docx":
			docText, derr := attachment.ExtractDocxText(data)
			if derr != nil {
				s.logger.Warn("httpchat: docx extract failed", "name", att.Name, "size", len(data), "err", derr)
				text = appendInlineFile(text, fmt.Sprintf("[docx: %s — extraction failed: %v]", att.Name, derr))
			} else {
				text = appendInlineFile(text, fmt.Sprintf("[docx: %s]\n%s", att.Name, docText))
			}
		case "text":
			// Mirror Telegram's text-doc inlining: fenced code block keeps the
			// model honest about where the file starts and ends, the filename
			// gives it something to cite.
			text = appendInlineFile(text, fmt.Sprintf("[file: %s]\n```\n%s\n```", att.Name, strings.ReplaceAll(string(data), "\r\n", "\n")))
		default:
			s.logger.Warn("httpchat: unknown attachment kind", "kind", att.Kind, "name", att.Name)
		}
	}

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	sink := &sseSink{w: w, flusher: flusher}

	// SSE keep-alive: every ~10s emit a comment line so intermediary
	// proxies (Caddy, Cloudflare, the user's own corp proxy) don't kill
	// the connection as "stale" during long cortex turns (extended
	// thinking, slow tool calls) where no real frame goes out for many
	// seconds. Comment lines start with ':' per the SSE spec and are
	// invisible to the EventSource client; sseSink.mu serialises them
	// with real emits.
	keepAliveCtx, stopKeepAlive := context.WithCancel(r.Context())
	defer stopKeepAlive()
	go sink.keepAlive(keepAliveCtx, 10*time.Second)

	// Decouple the turn's work context from the request context. A
	// browser refresh / tab close / network blip used to cascade
	// into the agent loop as `context canceled`, killing the
	// generation half-way and abandoning the assistant message
	// (chat_messages append happens at the END of the loop, after
	// the cancel had already fired). With WithoutCancel the work
	// completes server-side regardless — the user can refresh and
	// see the persisted reply on next history load. The 5-minute
	// hard cap stops genuinely stuck turns from running forever.
	workCtx, workCancel := context.WithTimeout(
		context.WithoutCancel(r.Context()),
		5*time.Minute,
	)
	defer workCancel()

	if err := s.gw.ProcessInboundForUser(workCtx, userID, soulID, s.transportName,
		[]bs.InboundMessage{{
			Text:             text,
			Images:           images,
			ReplyToMessageID: req.ReplyToMessageID,
			Ephemeral:        req.Source == "notebook",
		}}, sink); err != nil && workCtx.Err() == nil {
		s.logger.Warn("httpchat: process error", "error", err)
		sink.event("error", err.Error())
	}
	// A cut-off turn is not an error: the partial answer is already on
	// screen and persisted. Say so explicitly so the client can mark the
	// bubble as stopped rather than as complete.
	if workCtx.Err() != nil {
		sink.event("stopped", "")
	}
	sink.event("done", "")
}

// appendInlineFile glues an attached text/PDF rendering onto whatever
// the user typed, separating with a blank line so the model sees two
// distinct passages rather than a wall of text. Empty existing text
// (image-only or doc-only turn) skips the leading newlines.
func appendInlineFile(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "\n\n" + addition
}

// sseSink implements bs.ResponseSink plus the streaming sub-interfaces
// (TextStreamSink, ToolUseSink, ThinkingSink, MetaSink) used by the vaelum
// web cabinet to render the tool-use inspector.
//
// Frame format (one per "data:" line, terminated by \n\n):
//
//	{"type":"text","data":"chunk"}
//	{"type":"thinking","data":"chunk"}
//	{"type":"tool_use","id":"toolu_xxx","name":"...","input":{...}}
//	{"type":"tool_result","tool_use_id":"toolu_xxx","output":"...","is_error":false,"latency_ms":312}
//	{"type":"meta","session_id":"<uuid>","message_id":"<uuid>"}
//	{"type":"typing"}
//	{"type":"done"}
//	{"type":"error","data":"..."}
type sseSink struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
}

// emit writes one SSE frame from an arbitrary JSON-serializable payload.
// Always sets the "type" field via the caller's payload (the field is
// expected to be present).
func (s *sseSink) emit(payload any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "data: %s\n\n", data)
	s.flusher.Flush()
}

// event is the legacy two-field emit kept for typing/error/done frames.
func (s *sseSink) event(kind, data string) {
	if data == "" {
		s.emit(map[string]string{"type": kind})
		return
	}
	s.emit(map[string]string{"type": kind, "data": data})
}

// SendText is the batch-mode fallback. The streaming path (cb.OnText →
// SendTextDelta) is what the gateway actually uses for SSE clients; this
// only fires when the gateway falls back to a non-streaming provider that
// has no deltas to emit.
func (s *sseSink) SendText(ctx context.Context, text string) error {
	s.event("text", text)
	return nil
}

// SendVoice is a no-op — web chat is text-only.
func (s *sseSink) SendVoice(ctx context.Context, audio []byte) error {
	return nil
}

func (s *sseSink) SendTyping(ctx context.Context) error {
	s.event("typing", "")
	return nil
}

// SendTextDelta implements bs.TextStreamSink: each LLM text chunk becomes
// one SSE "text" frame. The vaelum front concatenates them into the
// current assistant message bubble.
func (s *sseSink) SendTextDelta(ctx context.Context, delta string) error {
	s.event("text", delta)
	return nil
}

// SendToolUse implements bs.ToolUseSink: emit a "tool_use" frame with the
// full assembled input JSON so the front can render a collapsible chip in
// the running answer.
func (s *sseSink) SendToolUse(ctx context.Context, id, name string, input json.RawMessage) error {
	if input == nil || len(input) == 0 || !json.Valid(input) {
		input = json.RawMessage("{}")
	}
	s.emit(map[string]any{
		"type":  "tool_use",
		"id":    id,
		"name":  name,
		"input": input,
	})
	return nil
}

// SendToolResult implements bs.ToolUseSink: emit a "tool_result" frame
// after the agent loop executes the tool. The front matches it against
// the prior tool_use by tool_use_id.
func (s *sseSink) SendToolResult(ctx context.Context, useID, output string, isError bool, latencyMs int) error {
	s.emit(map[string]any{
		"type":        "tool_result",
		"tool_use_id": useID,
		"output":      output,
		"is_error":    isError,
		"latency_ms":  latencyMs,
	})
	return nil
}

// SendThinking implements bs.ThinkingSink: stream extended-thinking deltas
// so the front can render a collapsed "thinking…" block in real time.
func (s *sseSink) SendThinking(ctx context.Context, delta string) error {
	s.event("thinking", delta)
	return nil
}

// SendMeta implements bs.MetaSink: emit a "meta" frame so the vaelum relay
// can link persisted tool_calls back to the assistant message that owns
// them. Called once at session bind (messageID=""), once after the loop
// persists the assistant response (both fields set).
func (s *sseSink) SendMeta(ctx context.Context, sessionID, messageID string) error {
	payload := map[string]string{"type": "meta"}
	if sessionID != "" {
		payload["session_id"] = sessionID
	}
	if messageID != "" {
		payload["message_id"] = messageID
	}
	s.emit(payload)
	return nil
}

// SendTurnStart implements bs.TurnStartSink: name this turn before anything
// streams, so the cabinet's stop button can address it and cannot stop a
// later one by accident. It rides the meta frame the client already parses.
func (s *sseSink) SendTurnStart(ctx context.Context, turnID string) error {
	s.emit(map[string]string{"type": "meta", "turn_id": turnID})
	return nil
}

// SendContextInfo implements bs.ContextInfoSink: emit a "context_info"
// frame so the cabinet can render a "🧠 N memories • M rules" chip on
// each assistant turn. Fired once per turn before any text/tool events.
func (s *sseSink) SendContextInfo(ctx context.Context, info bs.ContextInfo) error {
	payload := map[string]any{
		"type":     "context_info",
		"memories": info.Memories,
		"rules":    info.Rules,
	}
	if info.Strategy != "" {
		payload["strategy"] = info.Strategy
	}
	if len(info.MatchedRules) > 0 {
		payload["matched_rules"] = info.MatchedRules
	}
	if len(info.SuppressedRules) > 0 {
		payload["suppressed_rules"] = info.SuppressedRules
	}
	s.emit(payload)
	return nil
}

// SendTiming implements bs.TimingSink: emit a completed per-turn latency
// breakdown for debug/observability UIs.
func (s *sseSink) SendTiming(ctx context.Context, report bs.TimingReport) error {
	s.emit(map[string]any{
		"type":     "timing",
		"total_ms": report.TotalMs,
		"spans":    report.Spans,
	})
	return nil
}

// SendUsage implements bs.UsageSink: emit a "usage" frame with the
// cortex turn's token counts. The cabinet's window-size chip
// (next to the Reset button) reads it to show "🪟 N tokens" — a
// live indicator of how much the LLM context has grown.
func (s *sseSink) SendUsage(ctx context.Context, inputTokens, outputTokens int) error {
	s.emit(map[string]any{
		"type":          "usage",
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
	})
	return nil
}

// keepAlive writes an SSE comment line every `interval` until ctx is
// cancelled. Comment lines (starting with ':') are spec-compliant
// no-ops on the EventSource client side but keep TCP/proxy state
// fresh — without them, Caddy / Cloudflare / any L7 proxy can sever
// the connection mid-cortex-turn when nothing real has flowed for
// a while (extended thinking, long tool calls), and the browser
// then sees the stream end prematurely with no text yet.
func (s *sseSink) keepAlive(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			_, err := fmt.Fprint(s.w, ": keepalive\n\n")
			if err == nil {
				s.flusher.Flush()
			}
			s.mu.Unlock()
		}
	}
}
