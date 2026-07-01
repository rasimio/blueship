package agent

import (
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

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
