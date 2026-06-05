package openaicodex

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

// TestLiveVision hits the real codex / ChatGPT Responses endpoint to
// confirm the subscription surface accepts an input_image item (the open
// question behind the image-drop fix). Gated: runs only with
// CODEX_LIVE_TEST=1 and a readable token file (OPENAI_CODEX_TOKEN_FILE,
// default data/openai-codex-tokens.json). It sends a generated red PNG
// and asks for the dominant colour — a "red" answer proves end-to-end
// vision through the exact buildUserInput path.
//
//	CODEX_LIVE_TEST=1 OPENAI_CODEX_TOKEN_FILE=/path/tokens.json \
//	  go test ./internal/provider/openaicodex/ -run TestLiveVision -v
func TestLiveVision(t *testing.T) {
	if os.Getenv("CODEX_LIVE_TEST") != "1" {
		t.Skip("set CODEX_LIVE_TEST=1 to run the live codex vision probe")
	}

	tokenFile := os.Getenv("OPENAI_CODEX_TOKEN_FILE")
	if tokenFile == "" {
		tokenFile = "data/openai-codex-tokens.json"
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	tokens := NewTokenStore(tokenFile, logger)
	if err := tokens.Load(); err != nil {
		t.Fatalf("load token store %q: %v", tokenFile, err)
	}
	if !tokens.IsConfigured() {
		t.Fatalf("token store %q has no usable tokens", tokenFile)
	}

	provider := NewCompletionProvider(tokens, 90*time.Second, nil, logger)

	// 16x16 solid red PNG, base64'd — a real image the model can describe.
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: 220, G: 20, B: 20, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())

	req := bs.CompletionRequest{
		Model:     "gpt-5.5",
		System:    "You are a vision probe. Answer in one lowercase word.",
		MaxTokens: 2000,
		Effort:    "low",
		Messages: []bs.Message{{
			Role: "user",
			Content: []bs.ContentBlock{
				{Type: "text", Text: "What is the dominant colour of this image? One lowercase word."},
				{Type: "image", Source: &bs.ImageSource{Type: "base64", MediaType: "image/png", Data: b64}},
			},
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	resp, err := provider.Complete(ctx, req)
	if err != nil {
		t.Fatalf("codex vision request errored (surface may reject input_image): %v", err)
	}

	var out strings.Builder
	for _, b := range resp.Content {
		if b.Type == "text" {
			out.WriteString(b.Text)
		}
	}
	answer := strings.ToLower(strings.TrimSpace(out.String()))
	t.Logf("codex vision answer: %q (stop=%s)", answer, resp.StopReason)
	if answer == "" {
		t.Fatalf("empty answer — request accepted but no text content returned")
	}
	if !strings.Contains(answer, "red") {
		t.Fatalf("model did not identify the colour (got %q) — image may not have been seen", answer)
	}
}
