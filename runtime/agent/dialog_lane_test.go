package agent

import (
	"strings"
	"testing"
)

// The lane exists because the remainder design squeezed a production
// dialogue to 628 tokens: memory context and tool schemas grew, and the
// conversation — the one irreplaceable input — got what was left. These
// tests pin the inverted elasticity: dialogue owns its lane, the turn
// context flexes.

func TestDialogLaneSurvivesAFatTurnContext(t *testing.T) {
	system := strings.Repeat("персона ", 400)
	context := strings.Repeat("память строка ранга N\n", 2000)

	decision, trimmedCtx := effectiveDialogBudgetDecisionLane(20000, 8000, system, "", context, nil)

	if decision.Mode != dialogBudgetModeLane {
		t.Fatalf("mode = %q", decision.Mode)
	}
	if decision.DialogBudget != 8000 {
		t.Fatalf("the lane flexed: dialog budget = %d, want the full 8000 — that flex is the 628-token amnesia", decision.DialogBudget)
	}
	if decision.ContextTrimmedTokens <= 0 {
		t.Fatal("a context that cannot fit reported no trimming")
	}
	if !strings.Contains(trimmedCtx, "[context_trimmed:") {
		t.Fatal("the cut is invisible to the model; a truncated shelf must read as policy, not corruption")
	}
	if decision.PromptOverhead+decision.DialogBudget > 20000 {
		t.Fatalf("lane arithmetic overflows the ceiling: overhead %d + lane %d > 20000", decision.PromptOverhead, decision.DialogBudget)
	}
	// The head of the context is the valuable end — shelves render
	// best-first. The trim must keep the head, not the tail.
	if !strings.HasPrefix(trimmedCtx, "память строка ранга N") {
		t.Fatalf("trim dropped the head of the context: %q", trimmedCtx[:60])
	}
}

// A small context on a quiet turn must pass through untouched — the lane is
// a reservation, not a tax.
func TestDialogLaneLeavesASmallContextAlone(t *testing.T) {
	decision, ctx := effectiveDialogBudgetDecisionLane(20000, 8000, "system", "", "короткий контекст", nil)
	if decision.ContextTrimmedTokens != 0 || ctx != "короткий контекст" {
		t.Fatalf("small context was touched: trimmed=%d ctx=%q", decision.ContextTrimmedTokens, ctx)
	}
	if decision.DialogBudget != 8000 {
		t.Fatalf("dialog budget = %d", decision.DialogBudget)
	}
}

// System prompt and schemas are never trimmed — when they alone crowd the
// ceiling, the lane gives way and the turn is flagged, same terminal shape
// as the legacy fallback.
func TestDialogLaneYieldsToUntrimmableOverhead(t *testing.T) {
	system := strings.Repeat("схема ", 12000) // ~24K tokens > ceiling
	decision, _ := effectiveDialogBudgetDecisionLane(20000, 8000, system, "", "контекст", nil)
	if !decision.PromptOverheadExceedsBudget {
		t.Fatal("untrimmable overhead over the ceiling was not flagged")
	}
}

// lane=0 is the off switch: hosts that never opted in keep byte-identical
// remainder behaviour.
func TestDialogLaneZeroKeepsLegacyBehaviour(t *testing.T) {
	system, context := "system prompt", "turn context"
	legacy := effectiveDialogBudgetDecision(20000, system, "", context, nil)
	viaLane, ctx := effectiveDialogBudgetDecisionLane(20000, 0, system, "", context, nil)
	if viaLane != legacy || ctx != context {
		t.Fatalf("lane=0 diverged from legacy: %+v vs %+v", viaLane, legacy)
	}
}
