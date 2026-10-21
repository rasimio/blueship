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

// A draft that claims an action no tool performed goes back to the model
// once: it is not persisted or streamed, the model reads the feedback with
// its tools still offered, and its corrected reply is what goes out.
func TestResponseRevisionSendsTheDraftBackOnce(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "batch"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			store := &fakeMessageStore{}
			reg := bs.NewToolRegistry()
			saved := 0
			reg.Register("save", "test", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
				saved++
				return map[string]any{"saved": true}, nil
			})
			provider := &scriptedProvider{responses: []*bs.CompletionResponse{
				{Content: []bs.ContentBlock{{Type: "text", Text: "Saved it."}}, StopReason: "end_turn"},
				{Content: []bs.ContentBlock{{Type: "tool_use", ID: "call", Name: "save", Input: json.RawMessage(`{}`)}}, StopReason: "tool_use"},
				{Content: []bs.ContentBlock{{Type: "text", Text: "Saved it, for real."}}, StopReason: "end_turn"},
			}}
			loop := NewLoop(provider, store, reg, nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			var revisable []bool
			cfg := RunConfig{SessionID: "s", MaxTurns: 2, MaxTokens: 64, MessageBudget: 6000,
				ResponseValidator: func(_ context.Context, req bs.ResponseValidationRequest) (string, error) {
					revisable = append(revisable, req.Revisable)
					if len(req.Tools) == 0 {
						return "", &bs.ResponseRevision{Feedback: "No tool ran: call save or drop the claim."}
					}
					return req.Text, nil
				},
			}
			var text, streamed string
			var err error
			if stream {
				text, _, err = loop.RunStream(context.Background(), cfg, "remember this", &bs.StreamCallbacks{OnText: func(s string) { streamed += s }})
			} else {
				var result *RunResult
				result, err = loop.RunTracked(context.Background(), cfg, "remember this")
				if result != nil {
					text = result.Text
				}
			}
			if err != nil || text != "Saved it, for real." || saved != 1 {
				t.Fatalf("text=%q saved=%d err=%v", text, saved, err)
			}
			if stream && streamed != "Saved it, for real." {
				t.Fatalf("the draft was streamed: %q", streamed)
			}
			if len(revisable) != 2 || !revisable[0] || revisable[1] {
				t.Fatalf("revisable per check = %v, want [true false]", revisable)
			}
			for _, row := range store.appended {
				if row.Role == "assistant" && strings.Contains(bs.ExtractText(bs.NormalizeContent(row.Content)), "Saved it.") {
					t.Fatalf("the revised draft was persisted: %+v", row)
				}
			}
			if len(provider.requests) != 3 {
				t.Fatalf("%d LLM calls, want draft + tool call + answer", len(provider.requests))
			}
			second := provider.requests[1]
			if len(second.Tools) == 0 {
				t.Fatal("the revision withheld the tools it asks for")
			}
			readFeedback, sawDraft := false, false
			for _, m := range second.Messages {
				text := bs.ExtractText(bs.NormalizeContent(m.Content))
				sawDraft = sawDraft || (m.Role == "assistant" && text == "Saved it.")
				readFeedback = readFeedback || (sawDraft && m.Role == "user" && strings.Contains(text, "No tool ran: call save or drop the claim."))
			}
			if !readFeedback {
				t.Fatalf("the model never read the feedback after its draft: %+v", second.Messages)
			}
		})
	}
}

// Only one revision per run, and none for a draft with a pending tool call:
// a validator asking anyway stops publication instead of looping.
func TestResponseRevisionIsRefusedWhenTheDraftCannotBeRevised(t *testing.T) {
	store := &fakeMessageStore{}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		{Content: []bs.ContentBlock{{Type: "text", Text: "Saved it."}}, StopReason: "end_turn"},
		{Content: []bs.ContentBlock{{Type: "text", Text: "Saved it again."}}, StopReason: "end_turn"},
	}}
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := RunConfig{SessionID: "s", MaxTurns: 3, MaxTokens: 64, MessageBudget: 6000,
		ResponseValidator: func(context.Context, bs.ResponseValidationRequest) (string, error) {
			return "", &bs.ResponseRevision{Feedback: "again"}
		},
	}
	_, _, err := loop.RunStream(context.Background(), cfg, "remember this", &bs.StreamCallbacks{OnText: func(string) {}})
	if err == nil || !strings.Contains(err.Error(), "cannot be revised") || len(provider.requests) != 2 {
		t.Fatalf("err=%v calls=%d, want one revision then a refusal", err, len(provider.requests))
	}
	for _, row := range store.appended {
		if row.Role == "assistant" {
			t.Fatalf("a refused draft was persisted: %+v", row)
		}
	}
}
