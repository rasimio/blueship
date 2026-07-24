package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

type usageRecordingStore struct {
	fakeMessageStore
	records []bs.LLMUsageRecord
}

func (s *usageRecordingStore) RecordLLMUsage(_ context.Context, record bs.LLMUsageRecord) error {
	s.records = append(s.records, record)
	return nil
}

func TestEffectiveMessageBudgetUsesChatHistoryFallback(t *testing.T) {
	loop := &Loop{cfg: &bs.Config{Limits: bs.LimitsConfig{
		MaxContext:        262144,
		ChatMessageBudget: 6000,
		MinMessageBudget:  10000,
	}}}

	decision := loop.effectiveMessageBudget(RunConfig{Role: "cortex"}, "system prompt", nil)

	if decision.Budget != 6000 {
		t.Fatalf("expected chat message budget fallback, got %d", decision.Budget)
	}
	if decision.Source != "config.limits.chat_message_budget" {
		t.Fatalf("unexpected budget source %q", decision.Source)
	}
}

func TestEffectiveMessageBudgetUsesRunContextWindow(t *testing.T) {
	loop := &Loop{cfg: &bs.Config{Limits: bs.LimitsConfig{
		MaxContext:       262144,
		MinMessageBudget: 1,
	}}}

	decision := loop.effectiveMessageBudget(
		RunConfig{Role: "background", ContextWindow: 12288},
		"system prompt",
		nil,
	)

	want := 12288 - bs.EstimateTextTokens("system prompt")
	if decision.Budget != want {
		t.Fatalf("message budget = %d, want role context window budget %d", decision.Budget, want)
	}
	if decision.Source != "calculated.context_minus_prompt" {
		t.Fatalf("unexpected budget source %q", decision.Source)
	}
}

func TestRecordLLMUsageUsesRunContextWindow(t *testing.T) {
	store := &usageRecordingStore{}
	loop := &Loop{
		store: store,
		cfg:   &bs.Config{Limits: bs.LimitsConfig{MaxContext: 262144}},
	}

	loop.recordLLMUsage(
		context.Background(),
		RunConfig{SessionID: "session-1", Role: "recurring", ContextWindow: 12288},
		"ollama:gemma4:e4b",
		nil,
		nil,
		"",
		"",
		3000,
		"model_config.recurring.message_budget",
		"",
		0,
		0,
		bs.Usage{},
		"end_turn",
		time.Now(),
	)

	if len(store.records) != 1 {
		t.Fatalf("usage records = %d, want 1", len(store.records))
	}
	if got := store.records[0].MaxContext; got != 12288 {
		t.Fatalf("usage max_context = %d, want run context window 12288", got)
	}
}

func TestEffectiveDialogBudgetFallsBackWhenPromptOverheadExceedsBudget(t *testing.T) {
	decision := effectiveDialogBudgetDecision(
		3000,
		strings.Repeat("system ", 5000),
		"",
		strings.Repeat("context ", 1000),
		nil,
	)

	if !decision.PromptOverheadExceedsBudget {
		t.Fatalf("expected prompt overhead to exceed budget, got %+v", decision)
	}
	if decision.DialogBudget != 3000 {
		t.Fatalf("dialog budget = %d, want original configured budget 3000", decision.DialogBudget)
	}
	if decision.Mode != dialogBudgetModeDialogFallback {
		t.Fatalf("mode = %q, want %q", decision.Mode, dialogBudgetModeDialogFallback)
	}
}
