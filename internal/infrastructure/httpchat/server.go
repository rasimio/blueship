// Package httpchat is an HTTP transport that streams a soul's chat
// response as Server-Sent Events. It serves the Vaelum web platform: the
// vaelum backend relays an authenticated user's message here, and the SSE
// stream is piped straight back to the browser.
//
// The same server also hosts host-supplied internal-API routes via the
// Extras callback on HTTPChatConfig — arlene plugs its AME-associate
// endpoint in this way. All routes share the bearer-token middleware so
// the host's extras are authed without each handler re-implementing it.
package httpchat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/attachment"
	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/browser"
	"github.com/rasimio/blueship/internal/gateway"
)

// Server is the HTTP/SSE chat transport.
type Server struct {
	gw     *gateway.Gateway
	port   int
	token  string
	extras func(*http.ServeMux)
	reset  func(ctx context.Context, userID string) (string, string, error)
	logger *slog.Logger
}

// NewServer creates an HTTP chat server attached to an existing Gateway.
// token is the shared service token vaelum must present; empty disables
// auth. extras, when non-nil, is called once during Run with the server's
// mux so the host can mount additional routes (they share the bearer
// middleware). reset, when non-nil, exposes POST /api/internal/chat/reset
// — vaelum's web cabinet calls it to archive the active session and
// open a fresh one (equivalent of the Telegram /reset command).
func NewServer(gw *gateway.Gateway, port int, token string, extras func(*http.ServeMux), reset func(context.Context, string) (string, string, error), logger *slog.Logger) *Server {
	return &Server{gw: gw, port: port, token: token, extras: extras, reset: reset, logger: logger}
}

// Run starts the HTTP server. Blocks until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat", s.handleChat)
	if s.reset != nil {
		mux.HandleFunc("POST /api/internal/chat/reset", s.handleReset)
	}
	if s.extras != nil {
		s.extras(mux)
	}

	handler := http.Handler(mux)
	if s.token != "" {
		handler = s.requireBearer(handler)
	}

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

type chatRequest struct {
	UserID      string           `json:"user_id"`
	SoulID      string           `json:"soul_id"`
	Text        string           `json:"text"`
	Attachments []chatAttachment `json:"attachments,omitempty"`
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

type resetRequest struct {
	UserID string `json:"user_id"`
	SoulID string `json:"soul_id"`
}

type resetResponse struct {
	OldSessionID string `json:"old_session_id"`
	NewSessionID string `json:"new_session_id"`
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

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
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
			} else {
				text = appendInlineFile(text, fmt.Sprintf("[pdf: %s — %d pages]%s", att.Name, pages, pdfText))
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

	flusher, ok := w.(http.Flusher)
	if !ok {
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

	if err := s.gw.ProcessInboundForUser(workCtx, userID, soulID, "vaelum",
		[]bs.InboundMessage{{Text: text, Images: images}}, sink); err != nil {
		s.logger.Warn("httpchat: process error", "error", err)
		sink.event("error", err.Error())
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
	s.emit(payload)
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
