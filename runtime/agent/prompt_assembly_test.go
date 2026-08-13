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

func TestRunTrackedPromptOnlyInputIsSentButNotPersisted(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "assistant", Content: "previous reply"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{{
		Content:    []bs.ContentBlock{{Type: "text", Text: "new reply"}},
		StopReason: "end_turn",
	}}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, logger)
	result, err := loop.RunTracked(context.Background(), RunConfig{
		SessionID:       "s1",
		SystemPrompt:    "base system",
		Model:           "test-model",
		MaxTokens:       64,
		MaxTurns:        1,
		MessageBudget:   6000,
		PromptOnlyInput: true,
	}, "internal turn trigger")
	if err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}
	if result.Text != "new reply" {
		t.Fatalf("want final text, got %q", result.Text)
	}
	assertPromptOnlyRequest(t, provider.requests, "internal turn trigger")
	assertOnlyAssistantPersisted(t, store.appended, "new reply")
}

func TestRunStreamPromptOnlyInputIsSentButNotPersisted(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "assistant", Content: "previous reply"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{{
		Content:    []bs.ContentBlock{{Type: "text", Text: "streamed reply"}},
		StopReason: "end_turn",
	}}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, logger)
	text, _, err := loop.RunStream(context.Background(), RunConfig{
		SessionID:       "s1",
		SystemPrompt:    "base system",
		Model:           "test-model",
		MaxTokens:       64,
		MaxTurns:        1,
		MessageBudget:   6000,
		PromptOnlyInput: true,
	}, "internal stream trigger", nil)
	if err != nil {
		t.Fatalf("RunStream failed: %v", err)
	}
	if text != "streamed reply" {
		t.Fatalf("want final text, got %q", text)
	}
	assertPromptOnlyRequest(t, provider.requests, "internal stream trigger")
	assertOnlyAssistantPersisted(t, store.appended, "streamed reply")
}

func TestRunStreamFallbackPromptOnlyInputIsSentButNotPersisted(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "assistant", Content: "previous reply"}},
	}
	provider := &batchOnlyProvider{response: &bs.CompletionResponse{
		Content:    []bs.ContentBlock{{Type: "text", Text: "fallback reply"}},
		StopReason: "end_turn",
	}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, logger)
	text, _, err := loop.RunStream(context.Background(), RunConfig{
		SessionID:       "s1",
		SystemPrompt:    "base system",
		Model:           "test-model",
		MaxTokens:       64,
		MaxTurns:        1,
		MessageBudget:   6000,
		PromptOnlyInput: true,
	}, "internal fallback trigger", nil)
	if err != nil {
		t.Fatalf("RunStream fallback failed: %v", err)
	}
	if text != "fallback reply" {
		t.Fatalf("want final text, got %q", text)
	}
	assertPromptOnlyRequest(t, provider.requests, "internal fallback trigger")
	assertOnlyAssistantPersisted(t, store.appended, "fallback reply")
}

func TestRunStreamPromptOnlyEphemeralPersistsNothing(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "assistant", Content: "previous reply"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{{
		Content:    []bs.ContentBlock{{Type: "text", Text: "draft reply"}},
		StopReason: "end_turn",
	}}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, logger)
	text, _, err := loop.RunStream(context.Background(), RunConfig{
		SessionID:       "s1",
		SystemPrompt:    "base system",
		Model:           "test-model",
		MaxTokens:       64,
		MaxTurns:        1,
		MessageBudget:   6000,
		PromptOnlyInput: true,
		Ephemeral:       true,
	}, "internal draft trigger", nil)
	if err != nil {
		t.Fatalf("RunStream failed: %v", err)
	}
	if text != "draft reply" {
		t.Fatalf("want draft text, got %q", text)
	}
	assertPromptOnlyRequest(t, provider.requests, "internal draft trigger")
	if len(store.appended) != 0 {
		t.Fatalf("ephemeral draft persisted messages: %#v", store.appended)
	}
}

func TestRunStreamPromptOnlyCanMapEmptyVisibleOutputToNoOp(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "assistant", Content: "previous reply"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{{
		StopReason: "end_turn",
	}}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, logger)
	text, _, err := loop.RunStream(context.Background(), RunConfig{
		SessionID:            "s1",
		SystemPrompt:         "base system",
		Model:                "test-model",
		MaxTokens:            64,
		MaxTurns:             1,
		MessageBudget:        6000,
		PromptOnlyInput:      true,
		Ephemeral:            true,
		EmptyVisibleFallback: "[no-op]",
	}, "internal draft trigger", nil)
	if err != nil {
		t.Fatalf("RunStream failed: %v", err)
	}
	if text != "[no-op]" {
		t.Fatalf("empty autonomous output = %q, want no-op", text)
	}
	if len(store.appended) != 0 {
		t.Fatalf("empty autonomous draft persisted messages: %#v", store.appended)
	}
}

func TestRunStreamPromptOnlyMapsRefusalToNoOp(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "previous turn"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{{
		Content:    []bs.ContentBlock{{Type: "text", Text: "provider refusal copy"}},
		StopReason: "refusal",
	}}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, logger)
	text, _, err := loop.RunStream(context.Background(), RunConfig{
		SessionID:            "s1",
		SystemPrompt:         "base system",
		Model:                "test-model",
		MaxTokens:            64,
		MaxTurns:             1,
		MessageBudget:        6000,
		PromptOnlyInput:      true,
		Ephemeral:            true,
		EmptyVisibleFallback: "[no-op]",
	}, "internal draft trigger", nil)
	if err != nil {
		t.Fatalf("RunStream failed: %v", err)
	}
	if text != "[no-op]" {
		t.Fatalf("autonomous refusal = %q, want no-op", text)
	}
	if len(store.appended) != 0 {
		t.Fatalf("autonomous refusal persisted messages: %#v", store.appended)
	}
}

func TestRunTrackedPromptOnlyMapsRefusalToNoOp(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "previous turn"}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{{
		Content:    []bs.ContentBlock{{Type: "text", Text: "provider refusal copy"}},
		StopReason: "refusal",
	}}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, logger)
	result, err := loop.RunTracked(context.Background(), RunConfig{
		SessionID:            "s1",
		SystemPrompt:         "base system",
		Model:                "test-model",
		MaxTokens:            64,
		MaxTurns:             1,
		MessageBudget:        6000,
		PromptOnlyInput:      true,
		Ephemeral:            true,
		EmptyVisibleFallback: "[no-op]",
	}, "internal draft trigger")
	if err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}
	if result.Text != "[no-op]" {
		t.Fatalf("autonomous refusal = %q, want no-op", result.Text)
	}
	if len(store.appended) != 0 {
		t.Fatalf("autonomous refusal persisted messages: %#v", store.appended)
	}
}

func assertPromptOnlyRequest(t *testing.T, requests []bs.CompletionRequest, wantInput string) {
	t.Helper()
	if len(requests) != 1 {
		t.Fatalf("want one provider request, got %d", len(requests))
	}
	messages := requests[0].Messages
	if len(messages) != 2 {
		t.Fatalf("want persisted dialog plus prompt-only input, got %#v", messages)
	}
	got := messages[1]
	blocks := bs.NormalizeContent(got.Content)
	if got.Role != "user" || len(blocks) == 0 || blocks[0].Text != wantInput {
		t.Fatalf("prompt-only input = %#v, want role=user leading text=%q", got, wantInput)
	}
}

// turnContextOf returns the [turn_context] block a request carries in its
// messages. Which message holds it is an implementation detail — it is anchored
// to the current user turn, not the tail — so this scans from the back.
func turnContextOf(req bs.CompletionRequest) string {
	for m := len(req.Messages) - 1; m >= 0; m-- {
		blocks := bs.NormalizeContent(req.Messages[m].Content)
		for i := len(blocks) - 1; i >= 0; i-- {
			if blocks[i].Type == "text" && strings.Contains(blocks[i].Text, "[turn_context]") {
				return blocks[i].Text
			}
		}
	}
	return ""
}

func assertOnlyAssistantPersisted(t *testing.T, appended []bs.Message, wantText string) {
	t.Helper()
	if len(appended) != 1 {
		t.Fatalf("want only assistant append, got %#v", appended)
	}
	if appended[0].Role != "assistant" {
		t.Fatalf("persisted message role = %q, want assistant", appended[0].Role)
	}
	if got := bs.ExtractText(bs.NormalizeContent(appended[0].Content)); got != wantText {
		t.Fatalf("persisted assistant text = %q, want %q", got, wantText)
	}
}

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
	// The turn context rides at the tail of the message array, never in the
	// system prompt: anything per-turn in the system block invalidates the
	// prefix cache for the whole dialog behind it.
	if strings.Contains(provider.requests[0].System, "[turn_context]") {
		t.Fatalf("turn context must not be in the system prompt, got %q", provider.requests[0].System)
	}
	if provider.requests[0].System != "base system" {
		t.Fatalf("system prompt should be the bare cacheable head, got %q", provider.requests[0].System)
	}
	lastTurn1 := provider.requests[0].Messages[len(provider.requests[0].Messages)-1]
	if lastTurn1.Role != "user" {
		t.Fatalf("turn context should ride a user turn, got role %q", lastTurn1.Role)
	}
	turn1Blocks := bs.NormalizeContent(lastTurn1.Content)
	if len(turn1Blocks) < 2 || turn1Blocks[0].Text != "hello" {
		t.Fatalf("visible dialog should be preserved ahead of the turn context, got %#v", turn1Blocks)
	}
	if !strings.Contains(turn1Blocks[len(turn1Blocks)-1].Text, "[turn_context]") {
		t.Fatalf("turn context should be the trailing block, got %#v", turn1Blocks)
	}
	// The whole point of the move: the cacheable head is byte-identical across
	// turns even though the turn context changes between them.
	if provider.requests[0].System != provider.requests[1].System {
		t.Fatalf("system prompt must be byte-stable across turns: %q vs %q",
			provider.requests[0].System, provider.requests[1].System)
	}
	// And the property that actually buys the cache: a later turn must EXTEND
	// the earlier one, never rewrite it. If the turn context travelled with the
	// tail of the array it would rewrite the message the previous turn already
	// sent, and the prefix would stop matching right there.
	assertPrefixExtension(t, provider.requests[0].Messages, provider.requests[1].Messages)
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
	toolResults := 0
	var promptResult string
	for _, block := range promptBlocks {
		if block.Type == "tool_result" {
			toolResults++
			promptResult, _ = block.Content.(string)
		}
	}
	if toolResults != 1 {
		t.Fatalf("want one prompt tool result, got %#v", promptBlocks)
	}
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

func TestRunTrackedInjectsRecentToolObservationsInTurnContext(t *testing.T) {
	store := &fakeMessageStore{
		dialog: []bs.Message{{Role: "user", Content: "is this a notes conflict?"}},
		observations: []bs.ToolObservation{{
			Name:      "calendar_week",
			Input:     `{"date_from":"2026-07-09","date_to":"2026-07-09"}`,
			Output:    `[{"title":"Обучение ИИ","calendar":"Rasim"},{"title":"Школа ИИ","calendar":"gadjiew.rasim@gmail.com"}]`,
			CreatedAt: time.Date(2026, 7, 8, 14, 32, 0, 0, time.UTC),
		}},
	}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{{
		Content:    []bs.ContentBlock{{Type: "text", Text: "calendar conflict"}},
		StopReason: "end_turn",
		Usage:      bs.Usage{InputTokens: 30, OutputTokens: 2},
	}}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, logger)
	if _, err := loop.RunTracked(context.Background(), RunConfig{
		SessionID:      "s1",
		SystemPrompt:   "base system",
		Model:          "test-model",
		MaxTokens:      64,
		MaxTurns:       1,
		MessageBudget:  6000,
		SkipUserAppend: true,
	}, "ignored"); err != nil {
		t.Fatalf("RunTracked failed: %v", err)
	}

	if store.observationLoads != 1 {
		t.Fatalf("recent tool observations should be loaded once, got %d", store.observationLoads)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("want one provider call, got %d", len(provider.requests))
	}
	turnCtx := turnContextOf(provider.requests[0])
	for _, want := range []string{"[recent tool observations]", "calendar_week", "2026-07-08T14:32:00Z", "Обучение ИИ", "Школа ИИ"} {
		if !strings.Contains(turnCtx, want) {
			t.Fatalf("turn context missing %q:\n%s", want, turnCtx)
		}
	}
	// The observation is allowed to ride the trailing turn-context block, but it
	// must never be merged into the visible dialogue text itself.
	if len(provider.requests[0].Messages) != 1 {
		t.Fatalf("tool observation should not add dialogue turns: %#v", provider.requests[0].Messages)
	}
	dialogBlocks := bs.NormalizeContent(provider.requests[0].Messages[0].Content)
	if len(dialogBlocks) == 0 || dialogBlocks[0].Text != "is this a notes conflict?" {
		t.Fatalf("visible dialogue text should be untouched: %#v", dialogBlocks)
	}
	if strings.Contains(dialogBlocks[0].Text, "[recent tool observations]") {
		t.Fatalf("tool observation leaked into visible dialogue: %#v", dialogBlocks)
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
	if !strings.Contains(turnContextOf(provider.requests[1]), "none. No native tool_use calls are available") {
		t.Fatalf("continuation turn context should expose no-tools shelf: %q", turnContextOf(provider.requests[1]))
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
	turnCtx := turnContextOf(provider.requests[0])
	// The block now defers to the tool definitions instead of re-listing
	// names (the list was pure duplication of the schema surface); it must
	// still exist to scope tool availability to this turn.
	if !strings.Contains(turnCtx, "[available_tools]") || !strings.Contains(turnCtx, "in your tool definitions") {
		t.Fatalf("turn context missing available_tools contract: %q", turnCtx)
	}
	if strings.Contains(turnCtx, "- memory_search") || strings.Contains(turnCtx, "- browser_search") {
		t.Fatalf("turn context re-listed tool names: %q", turnCtx)
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
	turnCtx := turnContextOf(provider.requests[0])
	toolShelf := strings.LastIndex(turnCtx, "[available_tools]")
	staleHint := strings.Index(turnCtx, "memory_search")
	if toolShelf < 0 || staleHint < 0 || toolShelf <= staleHint {
		t.Fatalf("available tool shelf should follow stale context hints: %q", turnCtx)
	}
	if !strings.Contains(turnCtx, "none. No native tool_use calls are available") {
		t.Fatalf("turn context missing no-tools directive: %q", turnCtx)
	}
}

type scriptedProvider struct {
	requests  []bs.CompletionRequest
	responses []*bs.CompletionResponse
}

type batchOnlyProvider struct {
	requests []bs.CompletionRequest
	response *bs.CompletionResponse
}

func (p *batchOnlyProvider) Complete(_ context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	p.requests = append(p.requests, req)
	return p.response, nil
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
	dialog              []bs.Message
	observations        []bs.ToolObservation
	dialogLoads         int
	observationLoads    int
	lastDialogBudget    int
	lastAnchorToSummary bool
	rawLoads            int
	appended            []bs.Message
	appendedTokens      []int
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

func (s *fakeMessageStore) DialogMessagesForAPI(_ context.Context, _ string, maxTokens int, anchorToSummary bool) ([]bs.Message, error) {
	s.dialogLoads++
	s.lastDialogBudget = maxTokens
	s.lastAnchorToSummary = anchorToSummary
	return cloneMessages(s.dialog), nil
}

func (s *fakeMessageStore) RecentToolObservations(context.Context, string, time.Time, int) ([]bs.ToolObservation, error) {
	s.observationLoads++
	return append([]bs.ToolObservation(nil), s.observations...), nil
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

// assertPrefixExtension fails unless `next` starts with exactly `prev`: same
// messages, same order, byte-identical content. This is the invariant every
// prefix cache is priced on — the first message that differs ends the match and
// everything behind it is re-read at full price.
func assertPrefixExtension(t *testing.T, prev, next []bs.Message) {
	t.Helper()
	if len(next) < len(prev) {
		t.Fatalf("later turn shrank the prompt: %d messages after %d", len(next), len(prev))
	}
	for i := range prev {
		if prev[i].Role != next[i].Role {
			t.Fatalf("message %d changed role between turns: %q -> %q", i, prev[i].Role, next[i].Role)
		}
		before, err := json.Marshal(bs.NormalizeContent(prev[i].Content))
		if err != nil {
			t.Fatalf("marshal message %d: %v", i, err)
		}
		after, err := json.Marshal(bs.NormalizeContent(next[i].Content))
		if err != nil {
			t.Fatalf("marshal message %d: %v", i, err)
		}
		if string(before) != string(after) {
			t.Fatalf("message %d was rewritten between turns, breaking the cacheable prefix:\n before: %s\n after:  %s",
				i, before, after)
		}
	}
}

// The dialogue window anchors at the summary boundary ONLY on turns whose
// prompt actually carries the summary text. Split, the pair reproduces the
// silent-shrink failure: a boundary-cut window with nothing covering what
// fell off — which is what every failed summary load used to do, and what
// disabling the feature would have done to every session with an old row.
func TestDialogWindowAnchorTravelsWithTheSummaryText(t *testing.T) {
	run := func(compact string) *fakeMessageStore {
		t.Helper()
		store := &fakeMessageStore{}
		provider := &scriptedProvider{responses: []*bs.CompletionResponse{{
			Content:    []bs.ContentBlock{{Type: "text", Text: "ответ"}},
			StopReason: "end_turn",
		}}}
		loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{}, slog.Default())
		cfg := RunConfig{
			SessionID:      "s1",
			SystemPrompt:   "system",
			Model:          "test-model",
			MaxTokens:      64,
			MaxTurns:       1,
			MessageBudget:  5000,
			SkipUserAppend: true,
			CompactSummary: compact,
		}
		if _, err := loop.RunTracked(context.Background(), cfg, "вопрос"); err != nil {
			t.Fatalf("RunTracked: %v", err)
		}
		return store
	}

	if store := run(""); store.lastAnchorToSummary {
		t.Fatal("no summary in the prompt, yet the window anchored at the boundary — the model loses pre-boundary history with nothing in its place")
	}
	if store := run("сводка сессии"); !store.lastAnchorToSummary {
		t.Fatal("summary is in the prompt but the window did not anchor — the boundary and the text must travel together")
	}
}
