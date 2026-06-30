package gateway

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestInteractionTierPreparesCortexContextWithoutReflexLLM(t *testing.T) {
	userID := uuid.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := &Gateway{
		deps: &bs.Deps{
			Config: &bs.Config{
				Gateway: bs.GatewayConfig{InteractionTier: true},
			},
		},
		logger: logger,
	}

	preparerCalled := false
	us := &UserState{
		UserID: userID,
		ChatID: "telegram:1",
		Deps: &bs.Deps{
			UserID: userID,
			ReflexPreparer: func(ctx context.Context, userID, message, priorContext string) *bs.ReflexContext {
				preparerCalled = true
				return &bs.ReflexContext{
					FormattedTraces: "[memory] important trace",
					MemoriesCount:   1,
					Strategy:        "warm",
				}
			},
			RuleEngine: func(ctx context.Context, rc bs.RuleContext) []bs.ActiveRule {
				return []bs.ActiveRule{{
					ID:      "rule-1",
					Trigger: "when done",
					Action:  "close note",
					Tools:   []string{"note_close"},
				}}
			},
		},
	}

	timings := newTurnTimer()
	result := g.runReflexPipeline(context.Background(), us, "готово", "prior", timings)

	if !preparerCalled {
		t.Fatalf("interaction-tier preflight should still call ReflexPreparer for Cortex context")
	}
	if result.InjectedCtx != "[memory] important trace" {
		t.Fatalf("Cortex context missing AME traces: %#v", result.InjectedCtx)
	}
	if result.MemoriesCount != 1 || result.Strategy != "warm" || us.LastStrategy != "warm" {
		t.Fatalf("context metadata not propagated: result=%+v last_strategy=%q", result, us.LastStrategy)
	}
	if !strings.Contains(result.ReflexGuidance, "close note") {
		t.Fatalf("rule guidance missing:\n%s", result.ReflexGuidance)
	}
	if len(result.CortexTools) != 1 || result.CortexTools[0] != "note_close" {
		t.Fatalf("forced Cortex tools not propagated: %#v", result.CortexTools)
	}
	for _, span := range timings.Report().Spans {
		if span.Name == "reflex_llm" {
			t.Fatalf("interaction-tier context prep must not call the old reflex planner LLM")
		}
	}
}
