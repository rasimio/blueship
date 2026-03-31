package ws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"nhooyr.io/websocket"

	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/gateway"
)

// Server handles WebSocket connections for voice/desktop clients.
type Server struct {
	gw     *gateway.Gateway
	cfg    bs.WebSocketConfig
	logger *slog.Logger
}

// NewServer creates a WebSocket server attached to an existing Gateway.
func NewServer(gw *gateway.Gateway, cfg bs.WebSocketConfig, logger *slog.Logger) *Server {
	return &Server{gw: gw, cfg: cfg, logger: logger}
}

// Run starts the HTTP server with WebSocket upgrade. Blocks until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)

	addr := fmt.Sprintf(":%d", s.cfg.Port)
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	s.logger.Info("websocket server started", "addr", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("ws server: %w", err)
	}
	return nil
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Auth: bearer token.
	if s.cfg.Token != "" {
		token := r.Header.Get("Authorization")
		if token != "Bearer "+s.cfg.Token {
			// Also check query param for browser clients.
			token = r.URL.Query().Get("token")
			if token != s.cfg.Token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		s.logger.Warn("ws: accept failed", "error", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	s.logger.Info("ws: client connected", "remote", r.RemoteAddr)
	s.handleConnection(r.Context(), conn)
	s.logger.Info("ws: client disconnected", "remote", r.RemoteAddr)
}

// InMsg is the client→server JSON message.
type InMsg struct {
	Type   string `json:"type"`   // "text", "audio"
	Data   string `json:"data"`   // text content or base64 audio
	Format string `json:"format"` // audio format: "wav", "ogg", "webm"
}

// OutMsg is the server→client JSON message.
type OutMsg struct {
	Type  string `json:"type"`            // "text", "audio", "audio_chunk", "thinking", "error"
	Data  string `json:"data"`
	Seq   int    `json:"seq,omitempty"`   // chunk sequence number
	Final bool   `json:"final,omitempty"` // true = last chunk
}

func (s *Server) handleConnection(ctx context.Context, conn *websocket.Conn) {
	// Use owner chatID for voice connections.
	chatID := "voice:owner"

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != -1 {
				return // normal close
			}
			s.logger.Warn("ws: read error", "error", err)
			return
		}

		var msg InMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			writeJSON(ctx, conn, OutMsg{Type: "error", Data: "invalid JSON"})
			continue
		}

		var inbound bs.InboundMessage
		switch msg.Type {
		case "text":
			inbound.Text = msg.Data
		case "audio":
			audio, err := base64.StdEncoding.DecodeString(msg.Data)
			if err != nil {
				writeJSON(ctx, conn, OutMsg{Type: "error", Data: "invalid base64 audio"})
				continue
			}
			inbound.Audio = audio
		default:
			writeJSON(ctx, conn, OutMsg{Type: "error", Data: "unknown message type"})
			continue
		}

		sink := &wsSink{conn: conn, ctx: ctx, logger: s.logger}

		if err := s.gw.ProcessInbound(ctx, chatID, []bs.InboundMessage{inbound}, sink); err != nil {
			s.logger.Warn("ws: process error", "error", err)
			writeJSON(ctx, conn, OutMsg{Type: "error", Data: err.Error()})
		}
	}
}

// wsSink implements bs.ResponseSink for WebSocket transport.
type wsSink struct {
	conn   *websocket.Conn
	ctx    context.Context
	logger *slog.Logger
}

func (s *wsSink) SendText(ctx context.Context, text string) error {
	return writeJSON(ctx, s.conn, OutMsg{Type: "text", Data: text})
}

func (s *wsSink) SendVoice(ctx context.Context, audio []byte) error {
	encoded := base64.StdEncoding.EncodeToString(audio)
	return writeJSON(ctx, s.conn, OutMsg{Type: "audio", Data: encoded})
}

func (s *wsSink) SendVoiceChunk(ctx context.Context, audio []byte, seq int, final bool) error {
	encoded := base64.StdEncoding.EncodeToString(audio)
	return writeJSON(ctx, s.conn, OutMsg{Type: "audio_chunk", Data: encoded, Seq: seq, Final: final})
}

func (s *wsSink) SendTyping(ctx context.Context) error {
	return writeJSON(ctx, s.conn, OutMsg{Type: "thinking", Data: ""})
}

func writeJSON(ctx context.Context, conn *websocket.Conn, msg OutMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
