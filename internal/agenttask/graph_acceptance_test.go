package agenttask

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

func TestGraphAcceptanceUsesHostPromptAndAllBranchEvidence(t *testing.T) {
	reviewer := &reviewerStub{verdict: `{"met":true,"reason":"confirmed"}`}
	deps := evaluatorTestDeps(reviewer, nil)
	deps.Prompts = core.NewMapPromptStore(map[string]string{"background-graph-acceptance": "Host graph acceptance instructions"})
	task := criteriaTask("Produce a report and create a file")
	task.ExecutorVersion = 2
	traces := json.RawMessage(`[{"tool":"read","output":"source"},{"tool":"create_file","output":"receipt:123","error":""}]`)
	verdict := evaluateAcceptance(context.Background(), deps, task, "Complete report", traces)
	if !verdict.Met || len(reviewer.prompts) != 1 || reviewer.systems[0] != "Host graph acceptance instructions" {
		t.Fatal(verdict, reviewer)
	}
	if len(reviewer.budgets) != 1 || reviewer.budgets[0] != graphAcceptanceMaxTokens {
		t.Fatal("v2 review lost reasoning and verdict budget", reviewer.budgets)
	}
	var input struct {
		Scope    string            `json:"evidence_scope"`
		Traces   []json.RawMessage `json:"confirmed_step_tool_traces"`
		Criteria string            `json:"acceptance_criteria"`
	}
	if err := json.Unmarshal([]byte(reviewer.prompts[0]), &input); err != nil {
		t.Fatal(err)
	}
	if input.Scope != "all_confirmed_graph_steps" || len(input.Traces) != 2 || input.Criteria != *task.AcceptanceCriteria || !strings.Contains(string(input.Traces[1]), "receipt:123") {
		t.Fatal(input)
	}
	if strings.Contains(reviewer.prompts[0], "FINAL iteration only") {
		t.Fatal("legacy evidence framing", reviewer.prompts[0])
	}
}

func TestGraphAcceptanceFailsClosedWithoutHostPrompt(t *testing.T) {
	for _, prompts := range []core.PromptStore{nil, core.NewMapPromptStore(nil), core.NewMapPromptStore(map[string]string{"background-graph-acceptance": " "})} {
		reviewer := &reviewerStub{verdict: `{"met":true}`}
		deps := evaluatorTestDeps(reviewer, nil)
		deps.Prompts = prompts
		task := criteriaTask("Produce report")
		task.ExecutorVersion = 2
		verdict := evaluateAcceptance(context.Background(), deps, task, "Report", nil)
		if verdict.Met || !verdict.Unavailable || len(reviewer.prompts) != 0 {
			t.Fatal(verdict, reviewer)
		}
	}
}

func TestGraphAcceptanceInputKeepsSourceRecordSeparateFromActions(t *testing.T) {
	before := time.Now().UTC()
	input := graphAcceptanceInput(criteriaTask("Use catalog"), "Report", nil,
		map[string]struct{}{"catalog/item": {}}, map[string]struct{}{"catalog/item": {}, "catalog/other": {}})
	var parsed struct {
		ReviewedAt time.Time         `json:"reviewed_at"`
		Cited      []string          `json:"cited_url_keys"`
		Fetched    []string          `json:"fetched_url_keys"`
		Traces     []json.RawMessage `json:"confirmed_step_tool_traces"`
	}
	if err := json.Unmarshal([]byte(input), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Cited) != 1 || len(parsed.Fetched) != 2 || len(parsed.Traces) != 0 {
		t.Fatal(parsed)
	}
	if parsed.ReviewedAt.Before(before) || parsed.ReviewedAt.After(time.Now().UTC()) {
		t.Fatalf("review time is not the runtime observation time: %v", parsed.ReviewedAt)
	}
	if got := graphAcceptanceInput(criteriaTask("Report"), "Report", json.RawMessage(`broken`), nil, nil); got != "" {
		t.Fatal("invalid evidence accepted", got)
	}
}
