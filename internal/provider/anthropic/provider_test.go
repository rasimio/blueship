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

	bs "github.com/rasimio/blueship/internal/core"
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

func TestTrimTrailingAssistant(t *testing.T) {
	mk := func(roles ...string) []apiMessage {
		out := make([]apiMessage, len(roles))
		for i, r := range roles {
			out[i] = apiMessage{Role: r, Content: []apiContentBlock{{Type: "text", Text: "x"}}}
		}
		return out
	}
	cases := []struct {
		name        string
		in          []string
		wantLen     int
		wantDropped int
		wantLast    string
	}{
		{"ends with user", []string{"user", "assistant", "user"}, 3, 0, "user"},
		{"ends with assistant", []string{"user", "assistant"}, 1, 1, "user"},
		{"trailing tool-results then assistant", []string{"user", "assistant", "user", "assistant"}, 3, 1, "user"},
		{"two trailing assistants", []string{"user", "assistant", "assistant"}, 1, 2, "user"},
		{"empty", nil, 0, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := trimTrailingAssistant(mk(tc.in...))
			if len(got) != tc.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tc.wantLen)
			}
			if dropped != tc.wantDropped {
				t.Fatalf("dropped = %d, want %d", dropped, tc.wantDropped)
			}
			if tc.wantLast != "" && got[len(got)-1].Role != tc.wantLast {
				t.Fatalf("last role = %s, want %s", got[len(got)-1].Role, tc.wantLast)
			}
		})
	}
}

// lastWireRole sends a request whose stored conversation ends with an assistant
// message and returns the role of the last message that reached the wire.
func lastWireRole(t *testing.T, provider *Provider) string {
	t.Helper()
	var lastRole string
	provider.httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			var payload struct {
				Messages []struct {
					Role string `json:"role"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("unmarshal request: %v", err)
			}
			if len(payload.Messages) > 0 {
				lastRole = payload.Messages[len(payload.Messages)-1].Role
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}
	_, err := provider.Complete(context.Background(), bs.CompletionRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 128,
		Messages: []bs.Message{
			{Role: "user", Content: []bs.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []bs.ContentBlock{{Type: "text", Text: "orphaned trailing turn"}}},
		},
	})
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	return lastRole
}

func TestOAuthDropsTrailingAssistant(t *testing.T) {
	provider := NewOAuthProvider(
		func() (string, error) { return "tok", nil },
		time.Second, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if got := lastWireRole(t, provider); got != "user" {
		t.Fatalf("OAuth wire array ends with %q, want it trimmed to end with user", got)
	}
}

func TestAPIKeyKeepsTrailingAssistant(t *testing.T) {
	// The API-key surface tolerates prefill, so the guard is gated off there.
	provider := NewProvider("test-key", time.Second, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := lastWireRole(t, provider); got != "assistant" {
		t.Fatalf("API-key wire array ends with %q, want assistant left intact", got)
	}
}
