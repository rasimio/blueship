// Package httpchat is an HTTP transport that streams a soul's chat
// response as Server-Sent Events. It serves the Vaelum web platform: the
// vaelum backend relays an authenticated user's message here, and the SSE
// stream is piped straight back to the browser.
package httpchat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/gateway"
)

// Server is the HTTP/SSE chat transport.
type Server struct {
	gw     *gateway.Gateway
	port   int
	token  string
	logger *slog.Logger
}

// NewServer creates an HTTP chat server attached to an existing Gateway.
// token is the shared service token vaelum must present; empty disables auth.
func NewServer(gw *gateway.Gateway, port int, token string, logger *slog.Logger) *Server {
	return &Server{gw: gw, port: port, token: token, logger: logger}
}

// Run starts the HTTP server. Blocks until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat", s.handleChat)

	addr := fmt.Sprintf(":%d", s.port)
	srv := &http.Server{Addr: addr, Handler: mux}

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

type chatRequest struct {
	UserID string `json:"user_id"`
	SoulID string `json:"soul_id"`
	Text   string `json:"text"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	// Service-token auth — vaelum is the trusted caller.
	if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

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
	if req.Text == "" {
		http.Error(w, "empty text", http.StatusBadRequest)
		return
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
	if err := s.gw.ProcessInboundForUser(r.Context(), userID, soulID, "vaelum",
		[]bs.InboundMessage{{Text: req.Text}}, sink); err != nil {
		s.logger.Warn("httpchat: process error", "error", err)
		sink.event("error", err.Error())
	}
	sink.event("done", "")
}

// sseSink implements bs.ResponseSink, writing Server-Sent Events.
type sseSink struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
}

func (s *sseSink) event(kind, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, _ := json.Marshal(map[string]string{"type": kind, "data": data})
	fmt.Fprintf(s.w, "data: %s\n\n", payload)
	s.flusher.Flush()
}

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
