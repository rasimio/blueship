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

func TestRunTrackedUsesVisibleDialogAndCurrentToolScratchpad(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "hello"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		{
			Content: []bs.ContentBlock{{
				Type:  "tool_use",
				ID:    "call_1",
				Name:  "lookup",
				Input: json.RawMessage(`{"q":"hello"}`),
			}},
			StopReason: "tool_use",
			Usage:      bs.Usage{InputTokens: 10, OutputTokens: 3},
		},
		{
			Content:    []bs.ContentBlock{{Type: "text", Text: "done"}},
			StopReason: "end_turn",
			Usage:      bs.Usage{InputTokens: 20, OutputTokens: 2},
		},
	}}
	registry := bs.NewToolRegistry()
	registry.Register("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
		return map[string]string{"result": "found"}, nil
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, registry, nil, &bs.Config{}, logger)
	result, err := loop.RunTracked(context.Background(), RunConfig{
		SessionID:       "s1",
		SystemPrompt:    "base system",
		Model:           "test-model",
		MaxTokens:       64,
		MaxTurns:        3,
		MessageBudget:   6000,
		SkipUserAppend:  true,
		InjectedContext: "memory trace",
		ReflexGuidance:  "rule trace",
	}, "ignored")
	if err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}
	if result.Text != "done" {
		t.Fatalf("want final text, got %q", result.Text)
	}
	if store.dialogLoads != 1 {
		t.Fatalf("visible dialog should be loaded once, got %d", store.dialogLoads)
	}
	if store.rawLoads != 0 {
		t.Fatalf("raw MessagesForAPI should not be used for prompt history, got %d", store.rawLoads)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("want two provider calls, got %d", len(provider.requests))
	}
	if got := provider.requests[0].Messages[0].Content; got != "hello" {
		t.Fatalf("turn 1 should get visible dialog only, got %#v", got)
	}
	if strings.Contains(provider.requests[0].Messages[0].Content.(string), "[context]") {
		t.Fatalf("turn context leaked into user message: %#v", provider.requests[0].Messages[0])
	}
	if !strings.Contains(provider.requests[0].System, "[turn_context]") {
		t.Fatalf("turn context should be in system prompt, got %q", provider.requests[0].System)
	}
	if len(provider.requests[1].Messages) != 3 {
		t.Fatalf("turn 2 should include dialog + assistant tool_use + tool_result scratchpad, got %#v", provider.requests[1].Messages)
	}
	if !hasToolUse(provider.requests[1].Messages[1]) {
		t.Fatalf("turn 2 missing assistant tool_use scratchpad: %#v", provider.requests[1].Messages)
	}
	if !hasToolResult(provider.requests[1].Messages[2]) {
		t.Fatalf("turn 2 missing user tool_result scratchpad: %#v", provider.requests[1].Messages)
	}
}

func TestRunTrackedCompactsToolResultOnlyForPrompt(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "read this"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		{
			Content: []bs.ContentBlock{{
				Type:  "tool_use",
				ID:    "call_1",
				Name:  "browser_fetch",
				Input: json.RawMessage(`{"url":"https://example.com"}`),
			}},
			StopReason: "tool_use",
			Usage:      bs.Usage{InputTokens: 10, OutputTokens: 3},
		},
		{
			Content:    []bs.ContentBlock{{Type: "text", Text: "done"}},
			StopReason: "end_turn",
			Usage:      bs.Usage{InputTokens: 20, OutputTokens: 2},
		},
	}}
	longText := strings.Repeat("A", maxPromptBrowserFetchTextChars+2000)
	registry := bs.NewToolRegistry()
	registry.Register("browser_fetch", "fetch", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{
			"url":         "https://example.com",
			"title":       "Example",
			"text":        longText,
			"source_kind": "html",
		}, nil
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, registry, nil, &bs.Config{}, logger)
	if _, err := loop.RunTracked(context.Background(), RunConfig{
		SessionID:      "s1",
		SystemPrompt:   "base system",
		Model:          "test-model",
		MaxTokens:      64,
		MaxTurns:       3,
		MessageBudget:  6000,
		SkipUserAppend: true,
	}, "ignored"); err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}

	if len(provider.requests) != 2 {
		t.Fatalf("want two provider calls, got %d", len(provider.requests))
	}
	promptBlocks := bs.NormalizeContent(provider.requests[1].Messages[2].Content)
	if len(promptBlocks) != 1 {
		t.Fatalf("want one prompt tool result, got %#v", promptBlocks)
	}
	promptResult, _ := promptBlocks[0].Content.(string)
	if strings.Contains(promptResult, longText) {
		t.Fatalf("prompt tool result should be compacted")
	}
	if !strings.Contains(promptResult, `"truncated":true`) || !strings.Contains(promptResult, `"original_text_chars"`) {
		t.Fatalf("prompt tool result missing truncation provenance: %s", promptResult)
	}

	var rawPersisted string
	for _, msg := range store.appended {
		for _, block := range bs.NormalizeContent(msg.Content) {
			if block.Type == "tool_result" && block.Name == "browser_fetch" {
				rawPersisted, _ = block.Content.(string)
			}
		}
	}
	if !strings.Contains(rawPersisted, longText) {
		t.Fatalf("persisted tool result should keep raw output")
	}
}

type scriptedProvider struct {
	requests  []bs.CompletionRequest
	responses []*bs.CompletionResponse
}

func (p *scriptedProvider) Complete(_ context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	p.requests = append(p.requests, req)
	if len(p.responses) == 0 {
		return &bs.CompletionResponse{StopReason: "end_turn"}, nil
	}
	resp := p.responses[0]
	p.responses = p.responses[1:]
	return resp, nil
}

type fakeMessageStore struct {
	dialog      []bs.Message
	dialogLoads int
	rawLoads    int
	appended    []bs.Message
}

func (s *fakeMessageStore) Append(_ context.Context, _ string, msg bs.Message) error {
	s.appended = append(s.appended, msg)
	return nil
}

func (s *fakeMessageStore) AppendWithTokens(_ context.Context, _ string, msg bs.Message, _ int) error {
	s.appended = append(s.appended, msg)
	return nil
}

func (s *fakeMessageStore) MessagesForAPI(context.Context, string, int) ([]bs.Message, error) {
	s.rawLoads++
	return nil, nil
}

func (s *fakeMessageStore) DialogMessagesForAPI(context.Context, string, int) ([]bs.Message, error) {
	s.dialogLoads++
	return cloneMessages(s.dialog), nil
}

func (s *fakeMessageStore) AllMessagesForAPI(context.Context, string) ([]bs.Message, error) {
	return nil, nil
}

func (s *fakeMessageStore) CompactSession(context.Context, string, string, int) error {
	return nil
}

func (s *fakeMessageStore) CreateSession(context.Context, string, string) (string, error) {
	return "", nil
}

func (s *fakeMessageStore) CreateSessionWithSource(context.Context, string, string, string, string) (string, error) {
	return "", nil
}

func (s *fakeMessageStore) ArchiveSession(context.Context, string) error {
	return nil
}

func (s *fakeMessageStore) LatestAssistantMessageID(context.Context, string) (string, error) {
	return "", nil
}

func (s *fakeMessageStore) RecordLastInputTokens(context.Context, string, int) error {
	return nil
}

func (s *fakeMessageStore) RecordLLMUsage(context.Context, bs.LLMUsageRecord) error {
	return nil
}
