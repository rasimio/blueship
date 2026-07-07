package core

import "testing"

func TestApplyDefaultsSetsChatMessageBudget(t *testing.T) {
	var cfg Config
	cfg.ApplyDefaults()

	if cfg.Limits.ChatMessageBudget != 6000 {
		t.Fatalf("expected default chat message budget 6000, got %d", cfg.Limits.ChatMessageBudget)
	}
}

func TestThinkingBudgetForModelRef(t *testing.T) {
	if got := ThinkingBudgetForModelRef(ModelRef{}); got != 0 {
		t.Fatalf("missing model ref should inherit global budget, got %d", got)
	}
	if got := ThinkingBudgetForModelRef(ModelRef{Name: "claude-sonnet-5"}); got != -1 {
		t.Fatalf("DB role with zero thinking budget should disable thinking, got %d", got)
	}
	if got := ThinkingBudgetForModelRef(ModelRef{Name: "claude-sonnet-5", ThinkingBudget: 1024}); got != 1024 {
		t.Fatalf("explicit DB thinking budget should pass through, got %d", got)
	}
}
