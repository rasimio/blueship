package agent

import (
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestCalculateBudgetCapsChatHistory(t *testing.T) {
	loop := &Loop{cfg: &bs.Config{Limits: bs.LimitsConfig{
		MaxContext:        262144,
		ChatMessageBudget: 6000,
		MinMessageBudget:  10000,
	}}}

	budget := loop.calculateBudget("system prompt", nil)

	if budget != 6000 {
		t.Fatalf("expected chat message budget cap, got %d", budget)
	}
}
