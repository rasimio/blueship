package gateway

import (
	"os"
	"strings"
	"testing"
)

// /persona has to survive the step that discards abandoned wizards.
//
// Someone renaming their assistant is onboarded, so their FSM row is an
// edit in flight. The dispatcher falls through to the "throw away a
// half-finished wizard from the old mode" branch on purpose — and that
// branch triggered on any non-empty step. So /persona asked for a name,
// binned the run when the name arrived, tried to sign the person up
// again, and answered "у тебя уже есть ассистент": the one sentence
// guaranteed to be useless to somebody trying to rename theirs.
//
// A source test because the alternative is standing up the whole
// gateway, and what went wrong was one missing term in one condition.
func TestAbandonedWizardCleanupSpareEditRuns(t *testing.T) {
	body, err := os.ReadFile("bot_onboarding.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	src := string(body)

	const marker = `step != "" && g.flow().Mode == bs.OnboardingModeInstant`
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatal("the stale-wizard cleanup is gone; if it moved, move this test with it")
	}
	// The whole condition, back to the start of its line.
	lineStart := strings.LastIndexByte(src[:i], '\n') + 1
	condition := src[lineStart : i+len(marker)]

	if !strings.Contains(condition, "!onboarded") {
		t.Errorf("cleanup runs for onboarded users too: %q\n"+
			"an edit in flight is exactly what it would destroy", strings.TrimSpace(condition))
	}
}
