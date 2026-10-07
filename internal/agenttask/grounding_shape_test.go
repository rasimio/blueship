package agenttask

import (
	"context"
	"testing"

	"github.com/rasimio/blueship/internal/core"
)

type groundingShapeProvider struct{ stop, body string }

func (p groundingShapeProvider) Complete(context.Context, core.CompletionRequest) (*core.CompletionResponse, error) {
	return &core.CompletionResponse{StopReason: p.stop, Content: []core.ContentBlock{{Type: "text", Text: p.body}}}, nil
}

func TestGroundingOuterBraceRecoveryPreservesVerdictAndTruncationGuard(t *testing.T) {
	for _, tc := range []struct {
		stop, status string
		unavailable  bool
	}{
		{"end_turn", "grounded", false},
		{"end_turn", "ungrounded", false},
		{"max_tokens", "grounded", true},
	} {
		body := `{"claims":[{"claim":"price","claim_type":"numerical","status":"` + tc.status + `","issue":"source comparison","claim_type_note":""}]`
		deps := evaluatorTestDeps(groundingShapeProvider{stop: tc.stop, body: body}, nil)
		result := evaluateGroundingPart(context.Background(), deps, core.AgentTask{ExecutorVersion: 2}, "price", []ToolOutput{fetchRow("https://example.test/item", "Item", "source", 0, 6)}, "")
		if result.Unavailable != tc.unavailable || result.Met != (tc.stop == "end_turn" && tc.status == "grounded") {
			t.Fatal(tc, result)
		}
		if !tc.unavailable && (len(result.Claims) != 1 || result.Claims[0].Status != tc.status || result.Claims[0].Issue != "source comparison") {
			t.Fatal("claim altered while recovering delimiter", result)
		}
	}
}
