package tts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// An OpenAI-compatible endpoint is asked for the container each caller plays:
// OGG Opus for Telegram voice notes, MP3 for the macOS voice client, which
// cannot play Opus.
func TestOpenAICompatibleClientAsksForTheCallersFormat(t *testing.T) {
	var formats []string
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		formats = append(formats, payload["response_format"].(string))
		authorization = r.Header.Get("Authorization")
		if _, ok := payload["speed"]; ok {
			t.Error("a default speed must not be sent")
		}
		_, _ = w.Write([]byte("audio"))
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "dlk_key", 1.0, time.Second)
	if audio, err := client.Synthesize(context.Background(), "Привет", "polina", ""); err != nil || string(audio) != "audio" {
		t.Fatalf("opus: %q %v", audio, err)
	}
	if audio, err := client.SynthesizeMP3(context.Background(), "Привет", "polina", ""); err != nil || string(audio) != "audio" {
		t.Fatalf("mp3: %q %v", audio, err)
	}
	if len(formats) != 2 || formats[0] != "opus" || formats[1] != "mp3" {
		t.Fatalf("formats requested: %v", formats)
	}
	if authorization != "Bearer dlk_key" {
		t.Fatalf("authorization %q", authorization)
	}
}
