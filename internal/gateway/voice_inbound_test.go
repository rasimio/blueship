package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/provider/openai"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

type voiceFiles struct{ err error }

func (f voiceFiles) DownloadFile(context.Context, string, int64) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []byte("OggS voice"), nil
}

// transcriberAnswering is a real OpenAI-shaped transcriber pointed at a
// server that answers every note with text.
func transcriberAnswering(t *testing.T, text string) *openai.TranscriptionProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":` + jsonQuote(text) + `}`))
	}))
	t.Cleanup(srv.Close)
	return openai.NewTranscriptionProviderWithEndpoint(srv.URL, "m", 5*time.Second)
}

func jsonQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func voiceTestGateway(w *openai.TranscriptionProvider) *Gateway {
	return &Gateway{whisper: w, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// A voice message is never an empty turn: whatever went wrong with its words,
// the turn runs on a marker and the person gets an answer instead of silence.
func TestVoiceMessageThatYieldsNoWordsStillMakesATurn(t *testing.T) {
	voice := &telegram.Voice{FileID: "f", Duration: 25}
	cases := map[string]*Gateway{
		"transcriber hears nothing": voiceTestGateway(transcriberAnswering(t, "  ")),
		"no transcriber":            voiceTestGateway(nil),
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			text, visible := g.readVoiceIntoTurn(context.Background(), voiceFiles{}, 31, voice, "", "")
			if text != voiceUnheard || visible != voiceUnheard {
				t.Fatalf("text = %q visible = %q", text, visible)
			}
		})
	}
	g := voiceTestGateway(transcriberAnswering(t, "привет"))
	if text, _ := g.readVoiceIntoTurn(context.Background(), voiceFiles{err: errors.New("gone")}, 31, voice, "", ""); text != voiceUnheard {
		t.Fatalf("download failure: text = %q", text)
	}
}

func TestVoiceMessageWordsJoinTheTurn(t *testing.T) {
	g := voiceTestGateway(transcriberAnswering(t, "позвони маме"))
	text, visible := g.readVoiceIntoTurn(context.Background(), voiceFiles{}, 31, &telegram.Voice{FileID: "f"}, "подпись", "подпись")
	if text != "подпись\n\nпозвони маме" || visible != "подпись\n\nпозвони маме" {
		t.Fatalf("text = %q visible = %q", text, visible)
	}
}
