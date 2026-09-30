package gateway

import (
	"context"
	"strings"

	"github.com/rasimio/blueship/internal/transport/telegram"
)

// voiceUnheard stands in for a voice message no words came out of. The turn
// still runs on it, so the person gets an answer — at worst a request to say
// it again — instead of silence.
const voiceUnheard = "[voice message: no words could be made out of it — ask the person to repeat it or write it]"

// voiceTranscriber is the part of the speech-to-text provider a voice message
// needs.
type voiceTranscriber interface {
	IsConfigured() bool
	Transcribe(ctx context.Context, audio []byte, filename string) (string, error)
}

type telegramFileClient interface {
	DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, error)
}

// readVoiceIntoTurn puts what a voice message says into the turn.
//
// A voice message never leaves the turn empty. When the note cannot be
// downloaded or transcribed, or the transcriber hears nothing, the turn gets
// voiceUnheard instead: an empty turn is dropped, and to the person that is
// the bot ignoring them. It happened with a 25-second note that
// gpt-4o-mini-transcribe returned as empty text, without an error
// (2026-09-30).
func (g *Gateway) readVoiceIntoTurn(ctx context.Context, client telegramFileClient, chatID int64, voice *telegram.Voice, text, visibleText string) (string, string) {
	transcript := g.transcribeVoice(ctx, client, chatID, voice)
	if transcript == "" {
		return appendDocInline(text, voiceUnheard), appendVisibleTranscript(visibleText, voiceUnheard)
	}
	return appendDocInline(text, transcript), appendVisibleTranscript(visibleText, transcript)
}

// transcribeVoice returns the note's words, or "" with the reason logged.
func (g *Gateway) transcribeVoice(ctx context.Context, client telegramFileClient, chatID int64, voice *telegram.Voice) string {
	var whisper voiceTranscriber
	if g.whisper != nil {
		whisper = g.whisper
	}
	if whisper == nil || !whisper.IsConfigured() {
		g.logger.Warn("voice message: no transcriber configured", "chat_id", chatID)
		return ""
	}
	audio, err := client.DownloadFile(ctx, voice.FileID, 10*1024*1024)
	if err != nil {
		g.logger.Warn("failed to download voice", "chat_id", chatID, "error", err)
		return ""
	}
	transcript, err := whisper.Transcribe(ctx, audio, "voice.ogg")
	if err != nil {
		g.logger.Warn("failed to transcribe voice", "chat_id", chatID, "error", err)
		return ""
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		g.logger.Warn("voice message transcribed to no words", "chat_id", chatID,
			"duration_s", voice.Duration, "bytes", len(audio))
	}
	return transcript
}
