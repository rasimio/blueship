package agenttask

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/rasimio/blueship/internal/core"
)

// reviewerStub answers the acceptance reviewer with a fixed verdict and keeps
// the prompt it was handed.
type reviewerStub struct {
	verdict string
	prompts []string
	systems []string
}

func (s *reviewerStub) Complete(_ context.Context, req core.CompletionRequest) (*core.CompletionResponse, error) {
	s.systems = append(s.systems, req.System)
	for _, m := range req.Messages {
		if text, ok := m.Content.(string); ok {
			s.prompts = append(s.prompts, text)
		} else if blocks, ok := m.Content.([]core.ContentBlock); ok {
			for _, b := range blocks {
				s.prompts = append(s.prompts, b.Text)
			}
		}
	}
	return &core.CompletionResponse{
		StopReason: "end_turn",
		Content:    []core.ContentBlock{{Type: "text", Text: s.verdict}},
	}, nil
}

func evaluatorTestDeps(llm core.CompletionProvider, db func(string) (*sqlx.DB, error)) core.AgentDeps {
	cfg := &core.Config{}
	cfg.Models.Primary.Provider = "test"
	cfg.Models.Primary.Name = "test-model"
	return core.AgentDeps{
		LLM:    llm,
		DB:     db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: cfg,
	}
}

func criteriaTask(criteria string) core.AgentTask {
	return core.AgentTask{
		ID:                 uuid.New(),
		UserID:             uuid.New(),
		Title:              "Площадки для свадьбы",
		Strategy:           core.StrategyDirect,
		AcceptanceCriteria: &criteria,
	}
}

// A fetch record we could not READ is not a record of nothing fetched.
//
// Task 7ee9fd00 had opened 259 pages (227 in the iteration traces, 259 rows in
// tool_outputs, every one carrying requested_url) when three consecutive
// iterations were rejected with "browser_fetch was never called — citations
// are synthesised, not read". The evidence queries discarded their errors, so
// one unreadable record looked exactly like an empty one, and the model was
// told to go do the thing it had already done 259 times. Three of its twenty
// iterations went to a rejection it could not act on.
func TestAcceptanceSkipsCitationGatesWhenFetchRecordUnreadable(t *testing.T) {
	llm := &reviewerStub{verdict: `{"met": true, "reason": "report is complete and cited"}`}
	deps := evaluatorTestDeps(llm, func(string) (*sqlx.DB, error) {
		return nil, errors.New("context deadline exceeded")
	})
	task := criteriaTask("Не менее 10 URL-цитат с минимум 4 доменов.")
	result := strings.Join([]string{
		"# Площадки", "1. https://a.example/one", "2. https://b.example/two",
		"3. https://c.example/three", "4. https://d.example/four",
	}, "\n")

	v := evaluateAcceptance(context.Background(), deps, task, result, json.RawMessage(`[]`))

	if strings.Contains(v.Reason, "hard gate") {
		t.Fatalf("an unreadable fetch record must not be reported as fabricated citations: %q", v.Reason)
	}
	if !v.Met {
		t.Fatalf("with the gates skipped the reviewer's verdict stands, got met=false: %q", v.Reason)
	}
	if len(llm.prompts) == 0 {
		t.Fatal("the reviewer was never asked")
	}
	if strings.Contains(strings.Join(llm.prompts, "\n"), "FETCH RECORD") {
		t.Fatal("a record that could not be read must not be handed to the reviewer as machine-verified ground truth")
	}
}

// The gate itself is untouched: a task with a readable, genuinely empty record
// that cites URLs anyway is still citing pages it never opened.
func TestAcceptanceStillFailsFabricatedCitationsWhenRecordReadable(t *testing.T) {
	llm := &reviewerStub{verdict: `{"met": true, "reason": "looks fine"}`}
	deps := evaluatorTestDeps(llm, nil) // no DB wired => readable, empty
	task := criteriaTask("Не менее 10 URL-цитат с минимум 4 доменов.")
	result := "1. https://a.example/one\n2. https://b.example/two"

	v := evaluateAcceptance(context.Background(), deps, task, result, json.RawMessage(`[]`))

	if v.Met {
		t.Fatal("citing URLs with an empty fetch record must still fail")
	}
	if !strings.Contains(v.Reason, "hard gate") {
		t.Fatalf("expected the fabrication hard gate, got: %q", v.Reason)
	}
}

// The reviewer and the worker must be given the same contract. background-task
// tells the worker a documented negative is a passing result ("could not
// ground claim X; the following Y was verified: …"); the reviewer prompt said
// only "meets every part of the criteria" and "half-done work is not done", so
// an honest report of an unpublished fact was failed for the admission. Task
// 7ee9fd00 died on exactly that: no venue publishes whether its backup hall is
// heated, the report said so, and the gate read the sentence as the gap.
func TestAcceptanceReviewerIsToldDocumentedNegativesCount(t *testing.T) {
	llm := &reviewerStub{verdict: `{"met": true, "reason": "ok"}`}
	deps := evaluatorTestDeps(llm, nil)
	task := criteriaTask("Для каждой площадки подтверждён отапливаемый резервный зал.")

	evaluateAcceptance(context.Background(), deps, task, "отчёт без ссылок", json.RawMessage(`[]`))

	if len(llm.systems) == 0 {
		t.Fatal("the reviewer was never asked")
	}
	system := llm.systems[0]
	for _, want := range []string{
		"do not publish",                         // the exception exists
		"name what was checked",                  // and is evidence-bound
		"Silence about a criterion is met=false", // silence is not a negative
		"never anything the writer controls",     // structure clauses stay hard
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("reviewer system prompt is missing %q:\n%s", want, system)
		}
	}
}
