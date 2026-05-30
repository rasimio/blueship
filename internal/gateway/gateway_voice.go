package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
	"github.com/rasimio/blueship/session"
)

func (g *Gateway) emitTurnCompleted(us *UserState, sess *session.Session) {
	if g.deps.TurnCompletedHook == nil || sess == nil {
		return
	}
	sid, err := uuid.Parse(sess.ID)
	if err != nil {
		g.logger.Warn("turn_completed: invalid session id", "session_id", sess.ID, "error", err)
		return
	}
	go g.deps.TurnCompletedHook(bs.WithSoulID(context.Background(), us.SoulID), us.UserID, sid)
}

// isVoiceEnabled checks if the user has voice mode on.
func (g *Gateway) isVoiceEnabled(ctx context.Context, us *UserState) bool {
	if g.deps.Users == nil {
		return false
	}
	profile, err := g.deps.Users.GetByID(ctx, us.UserID.String())
	if err != nil {
		return false
	}
	return profile.VoiceEnabled()
}

// synthesizeAndSendVoice synthesizes TTS and sends audio via ResponseSink.
// If sink supports StreamingVoiceSink, uses sentence-level pipelining
// for lower latency (client starts playback before full audio is ready).
func (g *Gateway) synthesizeAndSendVoice(ctx context.Context, sink bs.ResponseSink, us *UserState, text string) {
	cfg := g.deps.Config
	voice := cfg.TTSVoice

	var instruct string
	if cfg.TTSInstructMapper != nil {
		instruct = cfg.TTSInstructMapper(us.LastStrategy)
	}
	if cfg.TTSTextCleaner != nil {
		text = cfg.TTSTextCleaner(text)
	}

	// Sentence-level pipelining for streaming transports.
	// Uses MP3 format (macOS/iOS compatible) instead of OGG Opus.
	if streamSink, ok := sink.(bs.StreamingVoiceSink); ok {
		mp3Provider, hasMP3 := cfg.TTS.(bs.TTSProviderMP3)
		synthesize := cfg.TTS.Synthesize
		if hasMP3 {
			synthesize = mp3Provider.SynthesizeMP3
		}

		sentences := splitSentences(text)
		if len(sentences) <= 1 {
			audio, err := synthesize(ctx, text, voice, instruct)
			if err != nil {
				g.logger.Warn("tts: synthesize failed", "error", err)
				return
			}
			streamSink.SendVoiceChunk(ctx, audio, 1, true)
			return
		}

		g.logger.Info("tts: streaming", "sentences", len(sentences), "text_len", len(text))
		for i, sentence := range sentences {
			audio, err := synthesize(ctx, sentence, voice, instruct)
			if err != nil {
				g.logger.Warn("tts: chunk synthesis failed", "sentence", i, "error", err)
				continue
			}
			if err := streamSink.SendVoiceChunk(ctx, audio, i+1, i == len(sentences)-1); err != nil {
				g.logger.Warn("tts: send chunk failed", "error", err)
				return
			}
		}
		return
	}

	// Batch mode for non-streaming transports (Telegram).
	g.synthesizeBatch(ctx, sink, text, voice, instruct)
}

func (g *Gateway) synthesizeBatch(ctx context.Context, sink bs.ResponseSink, text, voice, instruct string) {
	cfg := g.deps.Config
	g.logger.Info("tts: synthesizing", "text_len", len(text), "voice", voice, "text_preview", truncateStr(text, 200))

	audio, err := cfg.TTS.Synthesize(ctx, text, voice, instruct)
	if err != nil {
		g.logger.Warn("tts: synthesize failed", "error", err)
		return
	}
	if cfg.TTSConverter != nil {
		if converted, err := cfg.TTSConverter(audio); err == nil {
			audio = converted
		} else {
			g.logger.Warn("tts: convert failed", "error", err)
			return
		}
	}
	if err := sink.SendVoice(ctx, audio); err != nil {
		g.logger.Warn("tts: send voice failed", "error", err)
	}
}

// splitSentences splits text on sentence boundaries for TTS pipelining.
func splitSentences(text string) []string {
	var sentences []string
	var current []rune
	runes := []rune(text)

	for i, r := range runes {
		current = append(current, r)
		if (r == '.' || r == '!' || r == '?' || r == '…') && i+1 < len(runes) {
			next := runes[i+1]
			// End sentence if followed by space + uppercase or newline.
			if next == ' ' || next == '\n' {
				s := strings.TrimSpace(string(current))
				if len([]rune(s)) >= 10 { // min sentence length to avoid splitting abbreviations
					sentences = append(sentences, s)
					current = nil
				}
			}
		}
	}
	if len(current) > 0 {
		s := strings.TrimSpace(string(current))
		if s != "" {
			sentences = append(sentences, s)
		}
	}
	return sentences
}

// shouldSendVoice checks if voice response should be sent.
// Always true for streaming sinks (WebSocket voice transport).
// For Telegram, checks user preference (/voice toggle).
func (g *Gateway) shouldSendVoice(ctx context.Context, us *UserState, sink bs.ResponseSink) bool {
	if _, ok := sink.(bs.StreamingVoiceSink); ok {
		return true // voice transport always gets audio
	}
	return g.isVoiceEnabled(ctx, us)
}

// telegramSink implements bs.ResponseSink for Telegram transport. Each
// instance is bound to one bot's client at construction so replies land
// on the bot the user pinged (file IDs, callbacks, and inline keyboards
// are all bot-scoped in Telegram, so sends must use the same client).
type telegramSink struct {
	gw     *Gateway
	chatID int64
	client *telegram.Client // never nil in practice; set by newTelegramSink
}

// newTelegramSink builds a sink for one Telegram chat on one bot. bi must
// be non-nil for the sink to actually send anything — the legacy single-bot
// fallback path keeps bi pointing at the cfg.Transport.BotToken bot.
func (g *Gateway) newTelegramSink(canonicalChatID string, bi *botInstance) *telegramSink {
	var client *telegram.Client
	if bi != nil {
		client = bi.client
	}
	return &telegramSink{gw: g, chatID: tgChatID(canonicalChatID), client: client}
}

func (s *telegramSink) SendText(ctx context.Context, text string) error {
	if s.client == nil {
		return fmt.Errorf("telegramSink.SendText: no telegram client (chat %d)", s.chatID)
	}
	return s.client.SendLong(ctx, s.chatID, text)
}

func (s *telegramSink) SendVoice(ctx context.Context, audio []byte) error {
	chatID := fmt.Sprintf("%d", s.chatID)
	if s.gw.deps.Sender != nil {
		return s.gw.deps.Sender.SendVoice(ctx, chatID, audio)
	}
	return fmt.Errorf("telegramSink.SendVoice: no MessageSender configured")
}

func (s *telegramSink) SendTyping(ctx context.Context) error {
	if s.client == nil {
		return nil
	}
	return s.client.SendChatAction(ctx, s.chatID, "typing")
}

// SendAttachment implements bs.AttachmentSendSink. Routes by kind:
// images go through SendPhoto so TG renders the gallery preview,
// PDFs and text-shaped docs go through SendDocument so the chat
// shows a file icon with the filename. Unknown kinds fall back to
// SendDocument — a download is always better than nothing.
func (s *telegramSink) SendAttachment(ctx context.Context, rec bs.AttachmentRecord, data []byte) error {
	if s.client == nil {
		return fmt.Errorf("telegramSink.SendAttachment: no telegram client (chat %d)", s.chatID)
	}
	chatID := fmt.Sprintf("%d", s.chatID)
	name := rec.Name
	if name == "" {
		name = "file"
	}
	if rec.Kind == "image" {
		return s.client.SendPhoto(ctx, chatID, name, rec.Mime, data, "")
	}
	return s.client.SendDocument(ctx, chatID, name, rec.Mime, data)
}

// tgChatID extracts int64 from canonical "telegram:NNN" string.
func tgChatID(canonical string) int64 {
	var id int64
	fmt.Sscanf(canonical, "telegram:%d", &id)
	return id
}

// tgCanonical converts int64 Telegram chat ID to canonical string.
func tgCanonical(chatID int64) string {
	return fmt.Sprintf("telegram:%d", chatID)
}

const reflexConfidenceThreshold = 0.7
const preActionTimeout = 10 * time.Second
const maxPreActions = 2

// reflexPipelineResult groups everything runReflexPipeline computes so the
// caller can wire the structured pieces (memories + matched rules +
// strategy) into the SSE context_info frame, while keeping the prompt
// glue strings the agent loop actually consumes (injectedCtx /
