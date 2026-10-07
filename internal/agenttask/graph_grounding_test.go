package agenttask

import (
	"strings"
	"testing"

	"github.com/rasimio/blueship/internal/core"
)

func TestGraphGroundingRejectsUnsupportedDetailsDespiteHighAverage(t *testing.T) {
	claims := make([]ClaimGrounding, 18)
	for i := range claims {
		claims[i] = ClaimGrounding{Claim: "Observed price", ClaimType: "numerical", Status: "grounded"}
	}
	claims = append(claims,
		ClaimGrounding{Claim: "ATX 3.0", ClaimType: "architectural", Status: "partial", Issue: "Source confirms power, not this standard"},
		ClaimGrounding{Claim: "2 slots", ClaimType: "numerical", Status: "partial", Issue: "Dimensions do not establish slot count"})
	legacy := scoreGroundingVerdict(GroundingVerdict{Claims: claims})
	if !legacy.Met {
		t.Fatal("fixture must reproduce legacy acceptance", legacy)
	}
	v := strictGraphGrounding(legacy)
	if v.Met || v.Unavailable || !strings.Contains(v.Reason, "ATX 3.0") || !strings.Contains(v.Reason, "2 slots") || !graphGroundingRepairable(&v) {
		t.Fatal("unsupported details escaped targeted repair", v)
	}
	claims[len(claims)-1].Status = "ungrounded"
	v = strictGraphGrounding(scoreGroundingVerdict(GroundingVerdict{Claims: claims}))
	if v.Met || !graphGroundingRepairable(&v) {
		t.Fatal("unsupported assertion must fail acceptance but allow bounded correction", v)
	}
}

func TestGraphGroundingRequiresValidAudit(t *testing.T) {
	for _, claims := range [][]ClaimGrounding{
		nil,
		{{ClaimType: "numerical", Status: "unknown"}},
		{{ClaimType: "unknown", Status: "grounded"}},
	} {
		v := strictGraphGrounding(scoreGroundingVerdict(GroundingVerdict{Claims: claims}))
		if v.Met || !v.Unavailable || graphGroundingRepairable(&v) {
			t.Fatal("invalid audit did not fail closed", v)
		}
	}
	for _, kind := range []string{"attribution", "architectural", "numerical", "quote"} {
		v := strictGraphGrounding(scoreGroundingVerdict(GroundingVerdict{Claims: []ClaimGrounding{{ClaimType: kind, Status: "grounded"}}}))
		if !v.Met || v.Unavailable {
			t.Fatal("supported claim rejected", v)
		}
	}
	if groundingPrompt(core.AgentTask{ExecutorVersion: 1}) != groundingSystemPrompt || !strings.Contains(groundingPrompt(core.AgentTask{ExecutorVersion: 2}), "Explicit calculations") {
		t.Fatal("policy must be scoped to v2")
	}
}
