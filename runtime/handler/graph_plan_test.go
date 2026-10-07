package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

func TestGraphSearchStepCanInspectOnlyAuthorizedSources(t *testing.T) {
	plan := `{"steps":[{"id":"research","goal":"Find components","acceptance":"Sources found","kind":"read","tools":["browser_search"]},{"id":"final","goal":"Report","acceptance":"Complete","kind":"finalize","dependencies":["research"],"tools":[]}]}`
	for _, fetchAllowed := range []bool{false, true} {
		registry := core.NewToolRegistry()
		for _, name := range []string{"browser_search", "browser_fetch"} {
			if name == "browser_fetch" && !fetchAllowed {
				continue
			}
			registry.Register(name, name, json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
				t.Fatal("planning executed tool")
				return nil, nil
			})
			if err := registry.MarkReadOnly(name); err != nil {
				t.Fatal(err)
			}
		}
		steps, err := parseGraphPlan(plan, registry)
		if err != nil || len(steps) != 2 {
			t.Fatal(steps, err)
		}
		if slices.Contains(steps[0].Tools, "browser_fetch") != fetchAllowed || len(steps[1].Tools) != 0 {
			t.Fatal("tool scope expanded incorrectly", steps)
		}
	}
}

func graphPlanRegistry(t *testing.T) *core.ToolRegistry {
	t.Helper()
	r := core.NewToolRegistry()
	for _, name := range []string{"fetch", "write"} {
		r.Register(name, name, json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) { t.Fatal("planner executed tool"); return nil, nil })
	}
	if err := r.MarkReadOnly("fetch"); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGraphPlanValidatesDependenciesAndToolEffects(t *testing.T) {
	registry := graphPlanRegistry(t)
	valid := `{"steps":[{"id":"a","goal":"read a","acceptance":"verified a","kind":"read","dependencies":[],"tools":["fetch"]},{"id":"b","goal":"read b","acceptance":"verified b","kind":"read","dependencies":[],"tools":["fetch"]},{"id":"report","goal":"report","acceptance":"complete report","kind":"finalize","dependencies":["a","b"],"tools":["write"]}]}`
	steps, err := parseGraphPlan(valid, registry)
	if err != nil || len(steps) != 3 || len(steps[0].Dependencies) != 0 || len(steps[1].Dependencies) != 0 {
		t.Fatal(steps, err)
	}
	for name, input := range map[string]string{
		"effectful_read":     strings.Replace(valid, `"tools":["fetch"]`, `"tools":["write"]`, 1),
		"unknown_tool":       strings.Replace(valid, `"fetch"`, `"invented"`, 1),
		"uncovered_branch":   strings.Replace(valid, `["a","b"]`, `["a"]`, 1),
		"cycle":              strings.Replace(valid, `"dependencies":[]`, `"dependencies":["report"]`, 1),
		"forged_status":      strings.Replace(valid, `"id":"a"`, `"id":"a","status":"done"`, 1),
		"trailing":           valid + ` {}`,
		"finalizer_research": strings.Replace(valid, `"tools":["write"]`, `"tools":["fetch"]`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseGraphPlan(input, registry); err == nil {
				t.Fatal("accepted invalid plan")
			}
		})
	}
}

func TestGraphPlannerUsesHostPromptAndNoTools(t *testing.T) {
	provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
		if req.System != "host graph planning rules" || len(req.Tools) != 0 || len(req.Messages) != 1 {
			t.Fatalf("invalid planning call: %+v", req)
		}
		return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "thinking", Text: "PRIVATE_TEST_MARKER"}, {Type: "redacted_thinking", Text: "PRIVATE_TEST_MARKER"}, {Type: "text", Text: `{"steps":[{"id":"final","goal":"answer","acceptance":"complete answer","kind":"finalize","dependencies":[],"tools":[]}]}`}}}, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-plan": "host graph planning rules"})
	deps.Registry = graphPlanRegistry(t)
	deps.Config.Models.Primary.Name = "test-model"
	plan, err := NewBackground(time.UTC, nil, nil, nil).PlanGraph(context.Background(), core.AgentTask{Title: "Compare sources"}, deps, "Confirmed constraints")
	if err != nil || len(plan) != 1 {
		t.Fatal(plan, err)
	}
}

func TestGraphPlannerSeesExecutorResearchBudget(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			created := time.Now().Add(-2 * time.Minute)
			deadline := created.Add(9 * time.Minute)
			if expired {
				deadline = time.Now().Add(-time.Minute)
			}
			task := core.AgentTask{Title: "Choose options", CreatedAt: created, Deadline: &deadline}
			provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
				var input struct {
					Deadline  time.Time `json:"research_deadline"`
					Remaining int64     `json:"research_seconds_remaining"`
					Parallel  int       `json:"max_parallel_read_steps"`
				}
				if err := json.Unmarshal([]byte(core.ExtractText(core.NormalizeContent(req.Messages[0].Content))), &input); err != nil {
					t.Fatal(err)
				}
				expected := core.TaskResearchDeadline(task, 3*time.Minute)
				if !input.Deadline.Equal(*expected) || input.Parallel != core.BackgroundParallelReads {
					t.Fatalf("planner/executor budget mismatch: %+v", input)
				}
				if expired && input.Remaining != 0 {
					t.Fatal("negative or fictional remaining budget", input.Remaining)
				}
				if !expired && (input.Remaining < 239 || input.Remaining > 240) {
					t.Fatal("queue age/finalization reserve missing", input.Remaining)
				}
				return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"steps":[{"id":"final","goal":"answer","acceptance":"complete","kind":"finalize","dependencies":[],"tools":[]}]}`}}}, nil
			}}
			deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-plan": "plan"})
			deps.Registry = graphPlanRegistry(t)
			deps.Config.Models.Primary.Name = "test-model"
			deps.Config.Timeouts.TaskFinalizeReserve = 3 * time.Minute
			if _, err := NewBackground(time.UTC, nil, nil, nil).PlanGraph(context.Background(), task, deps, ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGraphPlannerRejectsUnexpectedResponseBlocks(t *testing.T) {
	for _, kind := range []string{"tool_use", "image", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			provider := &capturingProvider{respond: func(core.CompletionRequest) (*core.CompletionResponse, error) {
				return &core.CompletionResponse{Content: []core.ContentBlock{{Type: kind}, {Type: "text", Text: `{"steps":[{"id":"final","goal":"answer","acceptance":"complete","kind":"finalize","dependencies":[],"tools":[]}]}`}}}, nil
			}}
			deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-plan": "plan"})
			deps.Registry = graphPlanRegistry(t)
			deps.Config.Models.Primary.Name = "test-model"
			if _, err := NewBackground(time.UTC, nil, nil, nil).PlanGraph(context.Background(), core.AgentTask{}, deps, ""); err == nil {
				t.Fatal("unexpected non-text block accepted")
			}
		})
	}
}

func TestGraphPlannerTruncationIsNotRetriedAsTransport(t *testing.T) {
	complete := `{"steps":[{"id":"final","goal":"answer","acceptance":"complete","kind":"finalize","dependencies":[],"tools":[]}]}`
	for name, response := range map[string]*core.CompletionResponse{
		"max_tokens": {StopReason: "max_tokens", Content: []core.ContentBlock{{Type: "text", Text: complete[:40]}}},
		"malformed":  {StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: complete[:40]}}},
	} {
		t.Run(name, func(t *testing.T) {
			provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
				if req.MaxTokens != graphPlanMaxTokens {
					t.Fatalf("planner budget %d", req.MaxTokens)
				}
				return response, nil
			}}
			deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-plan": "plan"})
			deps.Registry = graphPlanRegistry(t)
			deps.Config.Models.Primary.Name = "test-model"
			_, err := NewBackground(time.UTC, nil, nil, nil).PlanGraph(context.Background(), core.AgentTask{}, deps, "")
			if err == nil {
				t.Fatal("incomplete plan accepted")
			}
			if retry, _ := core.TaskRetryPolicy(err); retry {
				t.Fatalf("identical planning request would be retried: %v", err)
			}
		})
	}
}

func TestGraphPlannerGetsOneCorrectionRound(t *testing.T) {
	noFinalizer := `{"steps":[{"id":"a","goal":"read a","acceptance":"verified a","kind":"read","dependencies":[],"tools":["fetch"]}]}`
	valid := `{"steps":[{"id":"a","goal":"read a","acceptance":"verified a","kind":"read","dependencies":[],"tools":["fetch"]},{"id":"report","goal":"report","acceptance":"complete","kind":"finalize","dependencies":["a"],"tools":[]}]}`
	for name, second := range map[string]string{"corrected": valid, "still_invalid": noFinalizer} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
				calls++
				if calls == 1 {
					return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: noFinalizer}}}, nil
				}
				last := core.ExtractText(core.NormalizeContent(req.Messages[len(req.Messages)-1].Content))
				if len(req.Messages) != 3 || core.ExtractText(core.NormalizeContent(req.Messages[1].Content)) != noFinalizer || !strings.Contains(last, "graph requires a finalizer") {
					t.Fatalf("correction lacks the rejected plan or reason: %+v", req.Messages)
				}
				return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: second}}}, nil
			}}
			deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-plan": "plan"})
			deps.Registry = graphPlanRegistry(t)
			deps.Config.Models.Primary.Name = "test-model"
			steps, err := NewBackground(time.UTC, nil, nil, nil).PlanGraph(context.Background(), core.AgentTask{Title: "Research"}, deps, "")
			if calls != 2 || (name == "corrected") != (err == nil && len(steps) == 2) {
				t.Fatal(calls, steps, err)
			}
		})
	}
}
