package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

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

func TestRunStreamRetriesEmptyVisibleMaxTokensWithoutReasoning(t *testing.T) {
	store := &fakeMessageStore{}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		{
			Content:    nil,
			StopReason: "max_tokens",
			Usage:      bs.Usage{InputTokens: 100, OutputTokens: 6144},
		},
		{
			Content:    []bs.ContentBlock{{Type: "text", Text: "works"}},
			StopReason: "end_turn",
			Usage:      bs.Usage{InputTokens: 100, OutputTokens: 2},
		},
	}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, logger)
	text, _, err := loop.RunStream(context.Background(), RunConfig{
		SessionID:      "s1",
		SystemPrompt:   "base system",
		Model:          "anthropic-oauth:claude-sonnet-5",
		MaxTokens:      2048,
		MaxTurns:       1,
		MessageBudget:  6000,
		SkipUserAppend: true,
		Effort:         "xhigh",
	}, "ignored", nil)
	if err != nil {
		t.Fatalf("RunStream failed: %v", err)
	}
	if text != "works" {
		t.Fatalf("want retry text, got %q", text)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("want initial call + no-reasoning retry, got %d", len(provider.requests))
	}
	if provider.requests[0].Effort != "xhigh" {
		t.Fatalf("initial request lost effort: %#v", provider.requests[0])
	}
	if provider.requests[1].Effort != "" || provider.requests[1].ThinkingMode != "off" || provider.requests[1].ThinkingBudget != 0 {
		t.Fatalf("retry should disable reasoning, got effort=%q thinking_mode=%q thinking_budget=%d",
			provider.requests[1].Effort, provider.requests[1].ThinkingMode, provider.requests[1].ThinkingBudget)
	}
	if len(store.appended) != 1 {
		t.Fatalf("want only final assistant append, got %#v", store.appended)
	}
	if got := bs.ExtractText(bs.NormalizeContent(store.appended[0].Content)); got != "works" {
		t.Fatalf("persisted assistant should be retry text, got %q", got)
	}
}

func TestRunTrackedAutoContinuesVisibleMaxTokensAndPersistsMergedAnswer(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "write a long answer"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		{
			Content:    []bs.ContentBlock{{Type: "text", Text: "part one"}},
			StopReason: "max_tokens",
			Usage:      bs.Usage{InputTokens: 100, OutputTokens: 10},
		},
		{
			Content:    []bs.ContentBlock{{Type: "text", Text: "part two"}},
			StopReason: "end_turn",
			Usage:      bs.Usage{InputTokens: 110, OutputTokens: 5},
		},
	}}
	registry := bs.NewToolRegistry()
	registry.Register("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
		return map[string]string{"ok": "true"}, nil
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, registry, nil, &bs.Config{}, logger)
	result, err := loop.RunTracked(context.Background(), RunConfig{
		SessionID:      "s1",
		SystemPrompt:   "base system",
		Model:          "test-model",
		MaxTokens:      64,
		MaxTurns:       1,
		MessageBudget:  6000,
		SkipUserAppend: true,
	}, "ignored")
	if err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}
	if result.Text != "part one\n\npart two" {
		t.Fatalf("want merged text, got %q", result.Text)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("want initial call + continuation, got %d", len(provider.requests))
	}
	if len(provider.requests[0].Tools) != 1 {
		t.Fatalf("first request should allow tools, got %#v", provider.requests[0].Tools)
	}
	if len(provider.requests[1].Tools) != 0 {
		t.Fatalf("continuation request should disable tools, got %#v", provider.requests[1].Tools)
	}
	if !strings.Contains(provider.requests[1].System, "none. No native tool_use calls are available") {
		t.Fatalf("continuation system prompt should expose no-tools shelf: %q", provider.requests[1].System)
	}
	messages := provider.requests[1].Messages
	if len(messages) != 3 {
		t.Fatalf("continuation prompt should include dialog + partial + directive, got %#v", messages)
	}
	if got := bs.ExtractText(bs.NormalizeContent(messages[1].Content)); got != "part one" {
		t.Fatalf("continuation prompt missing partial assistant text, got %q", got)
	}
	if got := bs.ExtractText(bs.NormalizeContent(messages[2].Content)); !strings.Contains(got, "[max_tokens_continuation]") {
		t.Fatalf("continuation prompt missing directive, got %q", got)
	}
	if len(store.appended) != 1 {
		t.Fatalf("want one merged assistant append, got %#v", store.appended)
	}
	if got := bs.ExtractText(bs.NormalizeContent(store.appended[0].Content)); got != "part one\n\npart two" {
		t.Fatalf("persisted assistant should be merged text, got %q", got)
	}
	if len(store.appendedTokens) != 1 || store.appendedTokens[0] != 15 {
		t.Fatalf("persisted token count should include both passes, got %#v", store.appendedTokens)
	}
}

func TestRunStreamAutoContinuesVisibleMaxTokensAndPersistsMergedAnswer(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "write a long answer"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		{
			Content:    []bs.ContentBlock{{Type: "text", Text: "alpha"}},
			StopReason: "max_tokens",
			Usage:      bs.Usage{InputTokens: 100, OutputTokens: 7},
		},
		{
			Content:    []bs.ContentBlock{{Type: "text", Text: "beta"}},
			StopReason: "end_turn",
			Usage:      bs.Usage{InputTokens: 108, OutputTokens: 3},
		},
	}}
	var streamed strings.Builder

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, logger)
	text, _, err := loop.RunStream(context.Background(), RunConfig{
		SessionID:      "s1",
		SystemPrompt:   "base system",
		Model:          "test-model",
		MaxTokens:      64,
		MaxTurns:       1,
		MessageBudget:  6000,
		SkipUserAppend: true,
	}, "ignored", &bs.StreamCallbacks{OnText: func(delta string) {
		streamed.WriteString(delta)
	}})
	if err != nil {
		t.Fatalf("RunStream failed: %v", err)
	}
	if text != "alpha\n\nbeta" {
		t.Fatalf("want merged text, got %q", text)
	}
	if streamed.String() != "alphabeta" {
		t.Fatalf("stream should emit both provider text blocks, got %q", streamed.String())
	}
	if len(provider.requests) != 2 {
		t.Fatalf("want initial call + continuation, got %d", len(provider.requests))
	}
	if len(store.appended) != 1 {
		t.Fatalf("want one merged assistant append, got %#v", store.appended)
	}
	if got := bs.ExtractText(bs.NormalizeContent(store.appended[0].Content)); got != "alpha\n\nbeta" {
		t.Fatalf("persisted assistant should be merged text, got %q", got)
	}
	if len(store.appendedTokens) != 1 || store.appendedTokens[0] != 10 {
		t.Fatalf("persisted token count should include both passes, got %#v", store.appendedTokens)
	}
}

func TestRunStreamFallbacksInsteadOfPersistingEmptyTerminalResponse(t *testing.T) {
	store := &fakeMessageStore{}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		{
			Content:    nil,
			StopReason: "end_turn",
			Usage:      bs.Usage{InputTokens: 10, OutputTokens: 0},
		},
	}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &bs.Config{}
	cfg.UI.ModelRefused = "fallback visible text"
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, cfg, logger)
	text, _, err := loop.RunStream(context.Background(), RunConfig{
		SessionID:      "s1",
		SystemPrompt:   "base system",
		Model:          "test-model",
		MaxTokens:      64,
		MaxTurns:       1,
		MessageBudget:  6000,
		SkipUserAppend: true,
	}, "ignored", nil)
	if err != nil {
		t.Fatalf("RunStream failed: %v", err)
	}
	if text != "fallback visible text" {
		t.Fatalf("want fallback text, got %q", text)
	}
	if len(store.appended) != 1 {
		t.Fatalf("want one assistant append, got %#v", store.appended)
	}
	if got := bs.ExtractText(bs.NormalizeContent(store.appended[0].Content)); got != "fallback visible text" {
		t.Fatalf("persisted assistant should be fallback text, got %q", got)
	}
}

func TestRunTrackedTimesOutHungTool(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "search"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		{
			Content: []bs.ContentBlock{{
				Type:  "tool_use",
				ID:    "call_1",
				Name:  "browser_search",
				Input: json.RawMessage(`{"query":"aeza alternatives"}`),
			}},
			StopReason: "tool_use",
			Usage:      bs.Usage{InputTokens: 10, OutputTokens: 3},
		},
		{
			Content:    []bs.ContentBlock{{Type: "text", Text: "fallback answer"}},
			StopReason: "end_turn",
			Usage:      bs.Usage{InputTokens: 20, OutputTokens: 2},
		},
	}}
	registry := bs.NewToolRegistry()
	registry.Register("browser_search", "search", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
		time.Sleep(200 * time.Millisecond)
		return map[string]string{"result": "too late"}, nil
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, registry, nil, &bs.Config{}, logger)
	started := time.Now()
	result, err := loop.RunTracked(context.Background(), RunConfig{
		SessionID:      "s1",
		SystemPrompt:   "base system",
		Model:          "test-model",
		MaxTokens:      64,
		MaxTurns:       3,
		MessageBudget:  6000,
		SkipUserAppend: true,
		ToolTimeout:    20 * time.Millisecond,
	}, "ignored")
	if err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 150*time.Millisecond {
		t.Fatalf("tool timeout did not unblock loop quickly enough: %s", elapsed)
	}
	if result.Text != "fallback answer" {
		t.Fatalf("want final text after timeout, got %q", result.Text)
	}
	if len(result.ToolTraces) != 1 || !result.ToolTraces[0].Error {
		t.Fatalf("want one errored tool trace, got %#v", result.ToolTraces)
	}
	if !strings.Contains(result.ToolTraces[0].Output, "timed out") {
		t.Fatalf("timeout trace missing timeout text: %#v", result.ToolTraces[0])
	}
}

func TestRunTrackedLoadsDialogWithEffectivePromptBudget(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "hello"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{{
		Content:    []bs.ContentBlock{{Type: "text", Text: "done"}},
		StopReason: "end_turn",
		Usage:      bs.Usage{InputTokens: 10, OutputTokens: 2},
	}}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, logger)
	cfg := RunConfig{
		SessionID:       "s1",
		SystemPrompt:    strings.Repeat("מערכת ", 200),
		Model:           "test-model",
		MaxTokens:       64,
		MaxTurns:        1,
		MessageBudget:   5000,
		SkipUserAppend:  true,
		InjectedContext: strings.Repeat("זיכרון ", 100),
		ReflexGuidance:  "rule trace",
	}
	if _, err := loop.RunTracked(context.Background(), cfg, "ignored"); err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}

	turnContext := buildTurnContextForTools(cfg.ReflexGuidance, cfg.InjectedContext, nil)
	wantBudget, wantOverhead := effectiveDialogBudget(cfg.MessageBudget, cfg.SystemPrompt, cfg.CompactSummary, turnContext, nil)
	if store.lastDialogBudget != wantBudget {
		t.Fatalf("DialogMessagesForAPI budget = %d, want %d (overhead %d)", store.lastDialogBudget, wantBudget, wantOverhead)
	}
	if store.lastDialogBudget >= cfg.MessageBudget {
		t.Fatalf("dialog budget should be reduced below total prompt budget, got %d >= %d", store.lastDialogBudget, cfg.MessageBudget)
	}
}

func TestRunTrackedTurnContextListsOnlyActualTools(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "hello"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{{
		Content:    []bs.ContentBlock{{Type: "text", Text: "done"}},
		StopReason: "end_turn",
		Usage:      bs.Usage{InputTokens: 10, OutputTokens: 2},
	}}}
	registry := bs.NewToolRegistry()
	registry.Register("memory_search", "memory", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
		return map[string]string{"ok": "true"}, nil
	})
	registry.Register("browser_search", "browser", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
		return map[string]string{"ok": "true"}, nil
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, registry, nil, &bs.Config{}, logger)
	if _, err := loop.RunTracked(context.Background(), RunConfig{
		SessionID:      "s1",
		SystemPrompt:   "base system",
		Model:          "test-model",
		MaxTokens:      64,
		MaxTurns:       1,
		MessageBudget:  6000,
		SkipUserAppend: true,
		ToolOverride:   []string{"browser_search"},
	}, "ignored"); err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}

	if len(provider.requests) != 1 {
		t.Fatalf("want one provider call, got %d", len(provider.requests))
	}
	if len(provider.requests[0].Tools) != 1 || provider.requests[0].Tools[0].Name != "browser_search" {
		t.Fatalf("provider tools = %#v, want only browser_search", provider.requests[0].Tools)
	}
	system := provider.requests[0].System
	if !strings.Contains(system, "[available_tools]") || !strings.Contains(system, "- browser_search") {
		t.Fatalf("system prompt missing actual tool shelf: %q", system)
	}
	if strings.Contains(system, "- memory_search") {
		t.Fatalf("system prompt listed unavailable tool: %q", system)
	}
}

func TestRunTrackedTurnContextSaysNoToolsWhenOverrideEmpty(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "hello"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{{
		Content:    []bs.ContentBlock{{Type: "text", Text: "done"}},
		StopReason: "end_turn",
		Usage:      bs.Usage{InputTokens: 10, OutputTokens: 2},
	}}}
	registry := bs.NewToolRegistry()
	registry.Register("memory_search", "memory", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
		return map[string]string{"ok": "true"}, nil
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, registry, nil, &bs.Config{}, logger)
	if _, err := loop.RunTracked(context.Background(), RunConfig{
		SessionID:       "s1",
		SystemPrompt:    "base system",
		Model:           "test-model",
		MaxTokens:       64,
		MaxTurns:        1,
		MessageBudget:   6000,
		SkipUserAppend:  true,
		InjectedContext: "A stale rule says to use memory_search.",
		ToolOverride:    []string{},
	}, "ignored"); err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}

	if len(provider.requests) != 1 {
		t.Fatalf("want one provider call, got %d", len(provider.requests))
	}
	if len(provider.requests[0].Tools) != 0 {
		t.Fatalf("provider tools = %#v, want none", provider.requests[0].Tools)
	}
	system := provider.requests[0].System
	toolShelf := strings.LastIndex(system, "[available_tools]")
	staleHint := strings.Index(system, "memory_search")
	if toolShelf < 0 || staleHint < 0 || toolShelf <= staleHint {
		t.Fatalf("available tool shelf should follow stale context hints: %q", system)
	}
	if !strings.Contains(system, "none. No native tool_use calls are available") {
		t.Fatalf("system prompt missing no-tools directive: %q", system)
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

func (p *scriptedProvider) StreamComplete(_ context.Context, req bs.CompletionRequest, cb *bs.StreamCallbacks) (*bs.CompletionResponse, error) {
	p.requests = append(p.requests, req)
	if len(p.responses) == 0 {
		return &bs.CompletionResponse{StopReason: "end_turn"}, nil
	}
	resp := p.responses[0]
	p.responses = p.responses[1:]
	if cb != nil && cb.OnText != nil {
		for _, block := range resp.Content {
			if block.Type == "text" && block.Text != "" {
				cb.OnText(block.Text)
			}
		}
	}
	return resp, nil
}

type fakeMessageStore struct {
	dialog           []bs.Message
	dialogLoads      int
	lastDialogBudget int
	rawLoads         int
	appended         []bs.Message
	appendedTokens   []int
}

func (s *fakeMessageStore) Append(_ context.Context, _ string, msg bs.Message) error {
	s.appended = append(s.appended, msg)
	return nil
}

func (s *fakeMessageStore) AppendWithTokens(_ context.Context, _ string, msg bs.Message, tokens int) error {
	s.appended = append(s.appended, msg)
	s.appendedTokens = append(s.appendedTokens, tokens)
	return nil
}

func (s *fakeMessageStore) MessagesForAPI(context.Context, string, int) ([]bs.Message, error) {
	s.rawLoads++
	return nil, nil
}

func (s *fakeMessageStore) DialogMessagesForAPI(_ context.Context, _ string, maxTokens int) ([]bs.Message, error) {
	s.dialogLoads++
	s.lastDialogBudget = maxTokens
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
