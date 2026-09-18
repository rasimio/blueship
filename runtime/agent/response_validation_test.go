package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestResponseValidationPreventsUnverifiedPersistenceAndStreaming(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, reject := range []bool{false, true} {
			name := "batch"
			if stream {
				name = "stream"
			}
			if reject {
				name += "_reject"
			}
			t.Run(name, func(t *testing.T) {
				store := &fakeMessageStore{}
				provider := &scriptedProvider{responses: []*bs.CompletionResponse{{Content: []bs.ContentBlock{{Type: "text", Text: "UNVERIFIED promise"}}, StopReason: "end_turn"}}}
				loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
				var streamed string
				cfg := RunConfig{SessionID: "s", MaxTurns: 1, MaxTokens: 64, MessageBudget: 6000,
					ResponseValidator: func(_ context.Context, req bs.ResponseValidationRequest) (string, error) {
						if streamed != "" {
							t.Fatal("provider text escaped before validation")
						}
						if req.Text != "UNVERIFIED promise" || req.UserText != "request" || len(req.Tools) != 0 {
							t.Fatalf("wrong request: %+v", req)
						}
						if reject {
							return "", errors.New("no receipt")
						}
						return "safe answer", nil
					},
				}
				var text string
				var err error
				if stream {
					text, _, err = loop.RunStream(context.Background(), cfg, "request", &bs.StreamCallbacks{OnText: func(s string) { streamed += s }})
				} else {
					var result *RunResult
					result, err = loop.RunTracked(context.Background(), cfg, "request")
					if result != nil {
						text = result.Text
					}
				}
				if reject && (err == nil || text != "" || streamed != "") {
					t.Fatalf("rejected response escaped: text=%q stream=%q err=%v", text, streamed, err)
				}
				if !reject && (err != nil || text != "safe answer") {
					t.Fatalf("replacement=%q err=%v", text, err)
				}
				for _, row := range store.appended {
					if row.Role != "assistant" {
						continue
					}
					if reject || bs.ExtractText(bs.NormalizeContent(row.Content)) != "safe answer" {
						t.Fatalf("unsafe durable row: %+v", row)
					}
				}
				if stream && !reject && streamed != "safe answer" {
					t.Fatalf("stream=%q", streamed)
				}
			})
		}
	}
}

func TestResponseValidationReceivesCompleteReceiptsAndPendingCalls(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "batch"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			store := &fakeMessageStore{}
			input := json.RawMessage(`{"id":"note","data":"` + strings.Repeat("x", 350) + `"}`)
			output := strings.Repeat("receipt", 160)
			reg := bs.NewToolRegistry()
			reg.Register("mutate", "test", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) { return map[string]any{"receipt": output}, nil })
			provider := &scriptedProvider{responses: []*bs.CompletionResponse{
				{Content: []bs.ContentBlock{{Type: "text", Text: "claimed before write"}, {Type: "tool_use", ID: "call", Name: "mutate", Input: input}}, StopReason: "tool_use"},
				{Content: []bs.ContentBlock{{Type: "text", Text: "claimed after write"}}, StopReason: "end_turn"},
			}}
			loop := NewLoop(provider, store, reg, nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			checks := 0
			cfg := RunConfig{SessionID: "s", MaxTurns: 3, MaxTokens: 64, MessageBudget: 6000,
				InitialToolResults: []bs.ToolExecutionResult{{Name: "preaction", Input: json.RawMessage(`{}`), Output: "preflight receipt"}},
				ResponseValidator: func(_ context.Context, req bs.ResponseValidationRequest) (string, error) {
					checks++
					if checks == 1 {
						if len(req.Tools) != 1 || req.Tools[0].Name != "preaction" || len(req.PendingTools) != 1 || req.PendingTools[0] != "mutate" {
							t.Fatalf("precall evidence=%+v", req)
						}
						return "checking", nil
					}
					if len(req.Tools) != 2 || len(req.PendingTools) != 0 {
						t.Fatalf("postcall evidence=%+v", req)
					}
					got := req.Tools[1]
					if string(got.Input) != string(input) || !strings.Contains(got.Output, output) || got.IsError {
						t.Fatalf("truncated/invalid receipt=%+v", got)
					}
					return "confirmed", nil
				},
			}
			var text string
			var err error
			var streamed string
			if stream {
				text, _, err = loop.RunStream(context.Background(), cfg, "request", &bs.StreamCallbacks{OnText: func(s string) { streamed += s }})
			} else {
				var result *RunResult
				result, err = loop.RunTracked(context.Background(), cfg, "request")
				if result != nil {
					text = result.Text
				}
			}
			if err != nil || checks != 2 || strings.Contains(text, "claimed") || (stream && strings.Contains(streamed, "claimed")) {
				t.Fatalf("text=%q stream=%q checks=%d err=%v", text, streamed, checks, err)
			}
		})
	}
}

func TestResponseValidationAlsoBuffersMaxTokenContinuations(t *testing.T) {
	store := &fakeMessageStore{}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		{Content: []bs.ContentBlock{{Type: "text", Text: "UNVERIFIED fragment"}}, StopReason: "max_tokens", Usage: bs.Usage{OutputTokens: 64}},
		{Content: []bs.ContentBlock{{Type: "text", Text: "continuation"}}, StopReason: "end_turn"},
	}}
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var streamed string
	cfg := RunConfig{SessionID: "s", MaxTurns: 2, MaxTokens: 64, MessageBudget: 6000, ResponseValidator: func(_ context.Context, req bs.ResponseValidationRequest) (string, error) {
		if streamed != "" {
			t.Fatal("unvalidated continuation escaped")
		}
		if !strings.Contains(req.Text, "UNVERIFIED fragment") || !strings.Contains(req.Text, "continuation") {
			t.Fatalf("validation missed merged text: %q", req.Text)
		}
		return "safe complete answer", nil
	}}
	text, _, err := loop.RunStream(context.Background(), cfg, "request", &bs.StreamCallbacks{OnText: func(s string) { streamed += s }})
	if err != nil || text != "safe complete answer" || streamed != text {
		t.Fatalf("text=%q stream=%q error=%v", text, streamed, err)
	}
}

func TestResponseValidationAppliesToStreamingBatchFallback(t *testing.T) {
	store := &fakeMessageStore{}
	provider := &batchOnlyProvider{response: &bs.CompletionResponse{Content: []bs.ContentBlock{{Type: "text", Text: "unverified"}}, StopReason: "end_turn"}}
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var streamed string
	cfg := RunConfig{SessionID: "s", MaxTurns: 1, MaxTokens: 64, MessageBudget: 6000, ResponseValidator: func(context.Context, bs.ResponseValidationRequest) (string, error) {
		return "verified replacement", nil
	}}
	text, _, err := loop.RunStream(context.Background(), cfg, "request", &bs.StreamCallbacks{OnText: func(s string) { streamed += s }})
	if err != nil || text != "verified replacement" || streamed != text {
		t.Fatalf("batch fallback text=%q stream=%q error=%v", text, streamed, err)
	}
	assertOnlyAssistantPersisted(t, store.appended[1:], text)
}
