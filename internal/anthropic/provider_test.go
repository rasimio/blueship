package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/core"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCompleteOmitsNameOnToolResult(t *testing.T) {
	provider := NewProvider("test-key", time.Second, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	provider.httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}

			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("unmarshal request: %v", err)
			}

			messages := payload["messages"].([]any)
			if len(messages) != 1 {
				t.Fatalf("expected 1 message, got %d", len(messages))
			}
			msg := messages[0].(map[string]any)
			blocks := msg["content"].([]any)
			if len(blocks) != 1 {
				t.Fatalf("expected 1 block, got %d", len(blocks))
			}
			block := blocks[0].(map[string]any)
			if _, ok := block["name"]; ok {
				t.Fatalf("tool_result must not contain name: %s", body)
			}
			if got := block["tool_use_id"]; got != "tool_123" {
				t.Fatalf("unexpected tool_use_id: %v", got)
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-5-20250929","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	resp, err := provider.Complete(context.Background(), bs.CompletionRequest{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 128,
		Messages: []bs.Message{{
			Role: "user",
			Content: []bs.ContentBlock{{
				Type:      "tool_result",
				Name:      "web_search",
				ToolUseID: "tool_123",
				Content:   map[string]any{"result": "done"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("unexpected stop reason: %s", resp.StopReason)
	}
}
