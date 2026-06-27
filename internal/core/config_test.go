package core

import "testing"

func TestApplyDefaultsSetsChatMessageBudget(t *testing.T) {
	var cfg Config
	cfg.ApplyDefaults()

	if cfg.Limits.ChatMessageBudget != 6000 {
		t.Fatalf("expected default chat message budget 6000, got %d", cfg.Limits.ChatMessageBudget)
	}
}
