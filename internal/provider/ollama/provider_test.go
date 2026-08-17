package ollama

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestBuildRequestSetsContextWindow(t *testing.T) {
	p := NewCompletionProvider("", time.Second, nil)

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

// Unset must stay off the wire entirely rather than serialise as a zero, which
// Ollama would read as "unload immediately" — the exact opposite of the
// default it is supposed to preserve.
func TestBuildRequestOmitsKeepAliveWhenUnset(t *testing.T) {
	p := NewCompletionProvider("", time.Second, nil)

	body, err := json.Marshal(p.buildRequest(bs.CompletionRequest{Model: "gemma4:e4b"}, false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "keep_alive") {
		t.Fatalf("keep_alive present with no setting: %s", body)
	}
}

func TestBuildRequestCarriesKeepAlive(t *testing.T) {
	for _, ka := range []any{-1, "30m"} {
		p := NewCompletionProvider("", time.Second, ka)
		req := p.buildRequest(bs.CompletionRequest{Model: "gemma4:e4b"}, false)
		if req.KeepAlive != ka {
			t.Fatalf("keep_alive = %v, want %v", req.KeepAlive, ka)
		}
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(body), `"keep_alive"`) {
			t.Fatalf("keep_alive %v did not reach the wire: %s", ka, body)
		}
	}
}
