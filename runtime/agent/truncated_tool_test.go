package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

// The live shape: the model writes its audit, opens attachment_create with the
// whole document in one argument, and the output cap lands inside that JSON.
// The provider returns the closed prefix — the key/value pairs that made it.
func truncatedToolCallResponse() *bs.CompletionResponse {
	return &bs.CompletionResponse{
		Content: []bs.ContentBlock{
			{Type: "text", Text: "<scratchpad>audit done, writing the file</scratchpad>"},
			{Type: "tool_use", ID: "toolu_cut", Name: "attachment_create", Input: json.RawMessage(`{"kind":"docx","name":"dossier.docx"}`)},
		},
		StopReason: "max_tokens",
		Usage:      bs.Usage{InputTokens: 100, OutputTokens: 8192},
	}
}

func newTruncatedToolLoop(t *testing.T, provider *scriptedProvider, store *fakeMessageStore) (*Loop, *int) {
	t.Helper()
	executed := 0
	registry := bs.NewToolRegistry()
	registry.Register("attachment_create", "attachment_create", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
		executed++
		return map[string]string{"id": "should-not-run"}, nil
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewLoop(provider, store, registry, nil, &bs.Config{}, logger), &executed
}

func truncatedToolRunConfig() RunConfig {
	return RunConfig{
		SessionID:      "s1",
		SystemPrompt:   "base system",
		Model:          "test-model",
		MaxTokens:      8192,
		MaxTurns:       1,
		MessageBudget:  6000,
		SkipUserAppend: true,
	}
}

func TestRunTrackedAnswersTruncatedToolCallWithErrorResultAndRetries(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "build the dossier"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		truncatedToolCallResponse(),
		{
			Content:    []bs.ContentBlock{{Type: "text", Text: "shortened and delivered [DONE]"}},
			StopReason: "end_turn",
			Usage:      bs.Usage{InputTokens: 120, OutputTokens: 20},
		},
	}}
	loop, executed := newTruncatedToolLoop(t, provider, store)

	result, err := loop.RunTracked(context.Background(), truncatedToolRunConfig(), "ignored")
	if err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}
	if *executed != 0 {
		t.Fatalf("a truncated tool call must not be executed, ran %d times", *executed)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("want the cut call answered and the model called again, got %d requests", len(provider.requests))
	}
	if !strings.Contains(result.Text, "shortened and delivered [DONE]") {
		t.Fatalf("retry text should reach the result, got %q", result.Text)
	}
	if len(provider.requests[1].Tools) != 1 {
		t.Fatalf("the retry must keep its tools so the call can be re-issued, got %#v", provider.requests[1].Tools)
	}

	// The retry prompt: dialog, the stump assistant turn, then the error
	// result — a tool_use without its result is rejected by the API.
	messages := provider.requests[1].Messages
	if len(messages) != 3 {
		t.Fatalf("retry prompt should be dialog + stump + error result, got %d messages", len(messages))
	}
	if messages[1].Role != "assistant" || !hasToolUseOutput(bs.NormalizeContent(messages[1].Content)) {
		t.Fatalf("retry prompt should carry the stump tool_use, got %#v", messages[1])
	}
	resultBlocks := bs.NormalizeContent(messages[2].Content)
	if messages[2].Role != "user" || len(resultBlocks) != 1 {
		t.Fatalf("retry prompt should end on one tool_result user turn, got %#v", messages[2])
	}
	block := resultBlocks[0]
	if block.Type != "tool_result" || block.ToolUseID != "toolu_cut" || !block.IsError {
		t.Fatalf("want an error tool_result for the cut call, got %#v", block)
	}
	resultText, _ := block.Content.(string)
	if !strings.Contains(resultText, "[tool_call_truncated]") || !strings.Contains(resultText, "max_tokens=8192") || !strings.Contains(resultText, "attachment_create") {
		t.Fatalf("error result should name the cause, the budget and the tool, got %q", resultText)
	}

	// Persisted the same way: stump, error result, final answer.
	if len(store.appended) != 3 {
		t.Fatalf("want stump + error result + final answer persisted, got %d", len(store.appended))
	}
	if store.appended[1].Role != "user" || !bs.NormalizeContent(store.appended[1].Content)[0].IsError {
		t.Fatalf("persisted second message should be the error tool_result, got %#v", store.appended[1])
	}

	// The audit trail says what happened, not "the model called a tool".
	if len(result.ToolTraces) != 1 {
		t.Fatalf("want one trace for the cut call, got %#v", result.ToolTraces)
	}
	trace := result.ToolTraces[0]
	if trace.Name != "attachment_create" || !trace.Error || !strings.Contains(trace.Output, "[tool_call_truncated]") {
		t.Fatalf("trace should record the cut call as an error, got %#v", trace)
	}
}

func TestRunTrackedStopsAnsweringTruncatedToolCallsAfterTheBound(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "build the dossier"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		truncatedToolCallResponse(),
		truncatedToolCallResponse(),
		truncatedToolCallResponse(),
		{
			Content:    []bs.ContentBlock{{Type: "text", Text: "never reached"}},
			StopReason: "end_turn",
		},
	}}
	loop, executed := newTruncatedToolLoop(t, provider, store)

	result, err := loop.RunTracked(context.Background(), truncatedToolRunConfig(), "ignored")
	if err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}
	if *executed != 0 {
		t.Fatalf("a truncated tool call must never be executed, ran %d times", *executed)
	}
	if len(provider.requests) != 1+maxTruncatedToolRecoveries {
		t.Fatalf("want the first call plus %d recoveries, got %d requests", maxTruncatedToolRecoveries, len(provider.requests))
	}
	if strings.Contains(result.Text, "never reached") {
		t.Fatalf("the bound should end the run on the last stump, got %q", result.Text)
	}
	if len(result.ToolTraces) != maxTruncatedToolRecoveries {
		t.Fatalf("want one error trace per answered stump, got %#v", result.ToolTraces)
	}
}

func TestRunStreamAnswersTruncatedToolCallWithErrorResultAndRetries(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "build the dossier"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		truncatedToolCallResponse(),
		{
			Content:    []bs.ContentBlock{{Type: "text", Text: "shortened and delivered"}},
			StopReason: "end_turn",
			Usage:      bs.Usage{InputTokens: 120, OutputTokens: 20},
		},
	}}
	loop, executed := newTruncatedToolLoop(t, provider, store)

	var seenID string
	var seenError bool
	text, traces, err := loop.RunStream(context.Background(), truncatedToolRunConfig(), "ignored", &bs.StreamCallbacks{
		OnToolResult: func(id, _ string, isError bool, _ int) {
			seenID, seenError = id, isError
		},
	})
	if err != nil {
		t.Fatalf("RunStream failed: %v", err)
	}
	if *executed != 0 {
		t.Fatalf("a truncated tool call must not be executed, ran %d times", *executed)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("want the cut call answered and the model called again, got %d requests", len(provider.requests))
	}
	if !strings.Contains(text, "shortened and delivered") {
		t.Fatalf("retry text should reach the result, got %q", text)
	}
	if seenID != "toolu_cut" || !seenError {
		t.Fatalf("the inspector should see the cut call as a failed result, got id=%q error=%v", seenID, seenError)
	}
	if len(traces) != 1 || !traces[0].Error {
		t.Fatalf("want one error trace for the cut call, got %#v", traces)
	}
	last := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	blocks := bs.NormalizeContent(last.Content)
	if last.Role != "user" || len(blocks) != 1 || blocks[0].Type != "tool_result" || !blocks[0].IsError {
		t.Fatalf("retry prompt should end on the error tool_result, got %#v", last)
	}
}
