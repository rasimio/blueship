package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/core"
)

func TestCompleteReturnsTextResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("key"); got != "test-key" {
			t.Fatalf("unexpected api key: %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"role":  "model",
					"parts": []any{map[string]any{"text": "hello from gemini"}},
				},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{"promptTokenCount": 11, "candidatesTokenCount": 7, "totalTokenCount": 18},
		})
	}))
	defer ts.Close()

	p := NewCompletionProvider("test-key", time.Second)
	p.generateURL = ts.URL + "/v1beta/models/%s:generateContent?key=%s"

	resp, err := p.Complete(context.Background(), bs.CompletionRequest{Model: "gemini-3-flash-preview", Messages: []bs.Message{{Role: "user", Content: "hi"}}, MaxTokens: 32, ThinkingBudget: -1})
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("unexpected stop reason: %q", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "hello from gemini" {
		t.Fatalf("unexpected content: %#v", resp.Content)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("unexpected usage: %#v", resp.Usage)
	}
}

func TestCompleteMarksFunctionCallAsToolUseEvenWhenFinishReasonIsStop(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"role": "model",
					"parts": []any{map[string]any{
						"functionCall":     map[string]any{"name": "lookup_weather", "args": map[string]any{"city": "Moscow"}},
						"thoughtSignature": "sig-123",
					}},
				},
				"finishReason": "STOP",
			}},
		})
	}))
	defer ts.Close()

	p := NewCompletionProvider("test-key", time.Second)
	p.generateURL = ts.URL + "/v1beta/models/%s:generateContent?key=%s"

	resp, err := p.Complete(context.Background(), bs.CompletionRequest{Model: "gemini-3-flash-preview", Messages: []bs.Message{{Role: "user", Content: "weather?"}}, MaxTokens: 32, ThinkingBudget: -1})
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("unexpected stop reason: %q", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" || resp.Content[0].Name != "lookup_weather" {
		t.Fatalf("unexpected content: %#v", resp.Content)
	}
	if string(resp.Content[0].Input) != `{"city":"Moscow"}` {
		t.Fatalf("unexpected tool input: %s", resp.Content[0].Input)
	}
	if resp.Content[0].ThoughtSignature != "sig-123" {
		t.Fatalf("unexpected thought signature: %q", resp.Content[0].ThoughtSignature)
	}
	if resp.Content[0].ID == "" {
		t.Fatalf("expected non-empty tool_use id")
	}
}

func TestCompleteReplaysThoughtSignatureOnToolRoundTrip(t *testing.T) {
	var calls int32
	var secondBody []byte

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		if idx == 2 {
			secondBody = append([]byte(nil), body...)
		}

		switch idx {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"candidates": []any{map[string]any{
					"content": map[string]any{
						"role": "model",
						"parts": []any{map[string]any{
							"functionCall":     map[string]any{"name": "current_time", "args": map[string]any{}},
							"thoughtSignature": "sig-roundtrip",
						}},
					},
					"finishReason": "STOP",
				}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"candidates": []any{map[string]any{
					"content": map[string]any{
						"role":  "model",
						"parts": []any{map[string]any{"text": "done"}},
					},
					"finishReason": "STOP",
				}},
			})
		}
	}))
	defer ts.Close()

	p := NewCompletionProvider("test-key", time.Second)
	p.generateURL = ts.URL + "/v1beta/models/%s:generateContent?key=%s"

	first, err := p.Complete(context.Background(), bs.CompletionRequest{Model: "gemini-3-flash-preview", Messages: []bs.Message{{Role: "user", Content: "time?"}}, MaxTokens: 64, ThinkingBudget: -1})
	if err != nil {
		t.Fatalf("first Complete error: %v", err)
	}
	if len(first.Content) != 1 || first.Content[0].ThoughtSignature != "sig-roundtrip" || first.Content[0].ID == "" {
		t.Fatalf("unexpected first response: %#v", first.Content)
	}

	_, err = p.Complete(context.Background(), bs.CompletionRequest{
		Model: "gemini-3-flash-preview",
		Messages: []bs.Message{
			{Role: "assistant", Content: first.Content},
			{Role: "user", Content: []bs.ContentBlock{{Type: "tool_result", Name: "current_time", ToolUseID: first.Content[0].ID, Content: map[string]any{"result": "01:41"}}}},
		},
		MaxTokens:      64,
		ThinkingBudget: -1,
	})
	if err != nil {
		t.Fatalf("second Complete error: %v", err)
	}

	var req generateRequest
	if err := json.Unmarshal(secondBody, &req); err != nil {
		t.Fatalf("unmarshal second request: %v", err)
	}
	if len(req.Contents) != 2 {
		t.Fatalf("unexpected contents: %#v", req.Contents)
	}
	modelParts := req.Contents[0].Parts
	if len(modelParts) != 1 || modelParts[0].FunctionCall == nil {
		t.Fatalf("unexpected model parts: %#v", modelParts)
	}
	if modelParts[0].ThoughtSignature != "sig-roundtrip" {
		t.Fatalf("thought signature not replayed: %#v", modelParts[0])
	}
}

func TestCompleteSendsExplicitThinkingBudgetWhenRequested(t *testing.T) {
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "ok"}}}, "finishReason": "STOP"}}})
	}))
	defer ts.Close()

	p := NewCompletionProvider("test-key", time.Second)
	p.generateURL = ts.URL + "/v1beta/models/%s:generateContent?key=%s"

	_, err := p.Complete(context.Background(), bs.CompletionRequest{Model: "gemini-3-flash-preview", Messages: []bs.Message{{Role: "user", Content: "hi"}}, MaxTokens: 8, ThinkingBudget: 0})
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}

	var req generateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if req.GenerationConfig == nil || req.GenerationConfig.ThinkingConfig == nil || req.GenerationConfig.ThinkingConfig.ThinkingBudget != 0 {
		t.Fatalf("expected explicit thinking budget 0, got %#v", req.GenerationConfig)
	}
}
