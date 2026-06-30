package agent

import (
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
