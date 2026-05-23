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
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logger.Warn("ws: accept failed", "error", err)
		return
	}
	conn.SetReadLimit(16 * 1024 * 1024) // 16MB for base64 audio
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	s.logger.Info("ws: client connected", "remote", r.RemoteAddr)
	s.handleConnection(r.Context(), conn, r.RemoteAddr)
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
	Type  string `json:"type"` // "text", "audio", "audio_chunk", "thinking", "error"
	Data  string `json:"data"`
	Seq   int    `json:"seq,omitempty"`   // chunk sequence number
	Final bool   `json:"final,omitempty"` // true = last chunk
}

// handleConnection dispatches to the legacy strictly-sequential loop or, when
// barge-in is enabled, the concurrent read-loop + turn-manager path. The
// remote address is threaded through so inbound/outbound logs can be
// correlated to a specific TCP connection — critical when multiple voice
// clients (or a reconnect storm) share the same chatID.
func (s *Server) handleConnection(ctx context.Context, conn *websocket.Conn, remote string) {
	if s.gw.BargeInEnabled() {
		s.handleConnectionBargeIn(ctx, conn, remote)
		return
	}
	s.handleConnectionLegacy(ctx, conn, remote)
}

// handleConnectionLegacy reads and processes one frame at a time — it does not
// read the next frame until the current turn finishes. No barge-in.
func (s *Server) handleConnectionLegacy(ctx context.Context, conn *websocket.Conn, remote string) {
	// Use owner chatID for voice connections.
	chatID := "voice:owner"

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != -1 {
				return // normal close
			}
			s.logger.Warn("ws: read error", "error", err, "remote", remote)
			return
		}

		var msg InMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			writeJSON(ctx, conn, OutMsg{Type: "error", Data: "invalid JSON"})
			continue
		}

		s.logger.Info("ws: inbound", "remote", remote, "type", msg.Type, "data_len", len(msg.Data))

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

		sink := &wsSink{conn: conn, ctx: ctx, logger: s.logger, remote: remote}

		if err := s.gw.ProcessInbound(ctx, chatID, []bs.InboundMessage{inbound}, sink); err != nil {
			s.logger.Warn("ws: process error", "error", err, "remote", remote)
			writeJSON(ctx, conn, OutMsg{Type: "error", Data: err.Error()})
		}
	}
}

// handleConnectionBargeIn runs the concurrent path: a read goroutine keeps
// draining frames into a channel while a turn is in flight, and the turn
// manager owns turn lifecycle, cancellation and interjection handling.
func (s *Server) handleConnectionBargeIn(ctx context.Context, conn *websocket.Conn, remote string) {
	w := newConnWriter(conn)
	tm := newTurnManager(s.gw, "voice:owner", w, s.logger)
	_ = remote // barge-in path has its own sink construction; logging hook lands later

	inbound := make(chan InMsg, 16)
	go func() {
		defer close(inbound)
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return // connection closed or read error
			}
			var msg InMsg
			if json.Unmarshal(data, &msg) != nil {
				w.write(ctx, OutMsg{Type: "error", Data: "invalid JSON"})
				continue
			}
			select {
			case inbound <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()

	tm.run(ctx, inbound)
}

// wsSink implements bs.ResponseSink (and bs.StreamingVoiceSink,
// bs.SpokenTextSink) for the WebSocket transport. The legacy path sets conn;
// the barge-in path sets writer (mutexed, multi-writer-safe) and turn (to
// record spoken text for interjection classification).
type wsSink struct {
	conn   *websocket.Conn // legacy path — direct writer
	ctx    context.Context
	logger *slog.Logger
	remote string // for log correlation across multiple concurrent sockets

	writer *connWriter // barge-in path — serialised writer
	turn   *turnHandle // barge-in path — spoken-text tracking
}

// emit sends one frame via whichever writer this sink was built with.
func (s *wsSink) emit(ctx context.Context, msg OutMsg) error {
	if s.writer != nil {
		return s.writer.write(ctx, msg)
	}
	return writeJSON(ctx, s.conn, msg)
}

func (s *wsSink) SendText(ctx context.Context, text string) error {
	return s.emit(ctx, OutMsg{Type: "text", Data: text})
}

func (s *wsSink) SendVoice(ctx context.Context, audio []byte) error {
	encoded := base64.StdEncoding.EncodeToString(audio)
	return s.emit(ctx, OutMsg{Type: "audio", Data: encoded})
}

func (s *wsSink) SendVoiceChunk(ctx context.Context, audio []byte, seq int, final bool) error {
	encoded := base64.StdEncoding.EncodeToString(audio)
	s.logger.Info("ws: send chunk", "remote", s.remote, "seq", seq, "final", final, "audio_bytes", len(audio), "encoded_bytes", len(encoded))
	return s.emit(ctx, OutMsg{Type: "audio_chunk", Data: encoded, Seq: seq, Final: final})
}

func (s *wsSink) SendTyping(ctx context.Context) error {
	return s.emit(ctx, OutMsg{Type: "thinking", Data: ""})
}

// NoteSpokenText records a streamed text chunk on the turn handle so the
// interjection classifier can see what the assistant is currently saying.
// No-op on the legacy path (turn is nil).
func (s *wsSink) NoteSpokenText(text string) {
	if s.turn != nil {
		s.turn.noteSpoken(text)
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, msg OutMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
