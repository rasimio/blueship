package ollama

import (
	"testing"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestBuildRequestSetsContextWindow(t *testing.T) {
	p := NewCompletionProvider("", time.Second)

	req := p.buildRequest(bs.CompletionRequest{
		Model:         "gemma4:e4b",
		MaxTokens:     16,
		ContextWindow: 4096,
	}, false)

	if got := req.Options["num_ctx"]; got != 4096 {
		t.Fatalf("num_ctx = %v, want %d", got, 4096)
	}
	if got := req.Options["num_predict"]; got != 16 {
		t.Fatalf("num_predict = %v, want %d", got, 16)
	}
}
