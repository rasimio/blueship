package ws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"nhooyr.io/websocket"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/gateway"
)

// connAuth carries the per-connection auth verdict from handleWS down into
// the read/dispatch loops. When `legacy` is true the connection runs the
// single-tenant voice:owner path (cfg.Token shared secret + ProcessInbound).
// When false it ran through cfg.ResolveDevice, so (userID, soulID) are
// populated and dispatch goes through ProcessInboundForUser.
type connAuth struct {
	legacy bool
	chatID string // "voice:owner" (legacy) or "voice:<userID>" (device-authed)
	userID uuid.UUID
	soulID uuid.UUID
}

// deviceTransport is the chatID prefix the gateway uses to key a separate
// UserState cache slot per (user, soul) for the voice WS path — distinct
// from "vaelum" (httpchat) so concurrent web and voice sessions for the
// same user don't share LoopBusy / Mu state.
const deviceTransport = "voice"

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
	auth, err := s.authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
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

	s.logger.Info("ws: client connected",
		"remote", r.RemoteAddr,
		"chat_id", auth.chatID,
		"device_authed", !auth.legacy)
	s.handleConnection(r.Context(), conn, r.RemoteAddr, auth)
	s.logger.Info("ws: client disconnected", "remote", r.RemoteAddr, "chat_id", auth.chatID)
}

// authenticate resolves the connection's auth verdict. Priority:
//  1. If cfg.ResolveDevice is wired, the Bearer header (or `?token=`) is a
//     per-user device token; resolve it to (userID, soulID) or 401.
//  2. Else fall back to the legacy shared cfg.Token (single-tenant dev).
//  3. Empty cfg.Token AND nil ResolveDevice = open server (legacy path).
func (s *Server) authenticate(r *http.Request) (connAuth, error) {
	token := bearerToken(r)
	if token == "" {
		token = r.URL.Query().Get("token")
	}

	if s.cfg.ResolveDevice != nil {
		if token == "" {
			return connAuth{}, fmt.Errorf("missing bearer token")
		}
		userID, soulID, err := s.cfg.ResolveDevice(r.Context(), token)
		if err != nil {
			s.logger.Info("ws: device auth rejected", "error", err, "remote", r.RemoteAddr)
			return connAuth{}, err
		}
		return connAuth{
			legacy: false,
			chatID: deviceTransport + ":" + userID.String(),
			userID: userID,
			soulID: soulID,
		}, nil
	}

	// Legacy single-tenant fallback.
	if s.cfg.Token != "" && token != s.cfg.Token {
		return connAuth{}, fmt.Errorf("invalid legacy token")
	}
	return connAuth{legacy: true, chatID: "voice:owner"}, nil
}

// bearerToken extracts a `Bearer <token>` value from the Authorization header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
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
func (s *Server) handleConnection(ctx context.Context, conn *websocket.Conn, remote string, auth connAuth) {
	if s.gw.BargeInEnabled() {
		s.handleConnectionBargeIn(ctx, conn, remote, auth)
		return
	}
	s.handleConnectionLegacy(ctx, conn, remote, auth)
}

// handleConnectionLegacy reads and processes one frame at a time — it does not
// read the next frame until the current turn finishes. No barge-in.
func (s *Server) handleConnectionLegacy(ctx context.Context, conn *websocket.Conn, remote string, auth connAuth) {
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

		var perr error
		if auth.legacy {
			perr = s.gw.ProcessInbound(ctx, auth.chatID, []bs.InboundMessage{inbound}, sink)
		} else {
			perr = s.gw.ProcessInboundForUser(ctx, auth.userID, auth.soulID, deviceTransport, []bs.InboundMessage{inbound}, sink)
		}
		if perr != nil {
			s.logger.Warn("ws: process error", "error", perr, "remote", remote)
			writeJSON(ctx, conn, OutMsg{Type: "error", Data: perr.Error()})
		}
	}
}

// handleConnectionBargeIn runs the concurrent path: a read goroutine keeps
// draining frames into a channel while a turn is in flight, and the turn
// manager owns turn lifecycle, cancellation and interjection handling.
func (s *Server) handleConnectionBargeIn(ctx context.Context, conn *websocket.Conn, remote string, auth connAuth) {
	w := newConnWriter(conn)
	tm := newTurnManager(s.gw, auth, w, s.logger)
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
