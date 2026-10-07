package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/core"
)

// neverStopStub fails the way production tasks stopped on 2026-09-23: an
// invalid plan, a branch that errors on every attempt, an action whose effect
// cannot be confirmed. The report must still be produced and announced.
type neverStopStub struct {
	mu       sync.Mutex
	plan     []core.TaskStep
	reportIn map[string]string
	executed map[string]int
}

func (*neverStopStub) DefaultTools() []string { return nil }
func (h *neverStopStub) PlanGraph(context.Context, core.AgentTask, core.AgentDeps, string) ([]core.TaskStep, error) {
	if h.plan == nil {
		return nil, errors.New("graph requires a finalizer")
	}
	return h.plan, nil
}
func (h *neverStopStub) ExecuteGraphStep(ctx context.Context, _ core.AgentTask, step core.TaskStep, inputs map[string]string, _ core.AgentDeps, save func(context.Context, json.RawMessage) error) (core.IterationResult, bool, error) {
	h.mu.Lock()
	h.executed[step.ID]++
	h.mu.Unlock()
	if step.Kind != "finalize" {
		_ = save(ctx, json.RawMessage(`{"phase":"executing","candidate_ready":false,"output":"Found: Vapi $0.05/min"}`))
		return core.IterationResult{}, false, errors.New("source unavailable")
	}
	h.mu.Lock()
	h.reportIn = inputs
	h.mu.Unlock()
	report := "# Market\n\nLeaders are Vapi and Retell.\n\n## Limitations\nSome data could not be confirmed."
	if err := save(ctx, json.RawMessage(`{"phase":"verified","candidate_ready":true,"output":`+mustJSON(report)+`}`)); err != nil {
		return core.IterationResult{}, false, err
	}
	return core.IterationResult{Output: report}, true, nil
}

func mustJSON(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func TestGraphTaskAlwaysReachesItsReport(t *testing.T) {
	for name, plan := range map[string][]core.TaskStep{
		"fallback_plan_failed_research": nil,
		"unconfirmed_action": {
			{ID: "save", Goal: "Save numbers", Acceptance: "Receipt", Kind: "action", MaxAttempts: 3},
			{ID: "report", Goal: "Report", Acceptance: "Complete", Kind: "finalize", Dependencies: []string{"save"}, MaxAttempts: 3},
		},
	} {
		t.Run(name, func(t *testing.T) {
			db := finalizationDB(t)
			if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid DEFAULT gen_random_uuid(),ADD COLUMN cadence text, ADD COLUMN description text, ADD COLUMN acceptance_criteria text, ADD COLUMN delegate_to text, ADD COLUMN tools text[] NOT NULL DEFAULT '{}', ADD COLUMN use_agents text[] NOT NULL DEFAULT '{}', ADD COLUMN session_id text`); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"027_agent_task_executor_version.sql", "031_agent_task_origin.sql"} {
				migration, err := migrations.ReadFile("sql/" + name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(string(migration)); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			store := core.NewAgentTaskStore(db)
			user := uuid.New()
			cfg := &core.Config{BackgroundTasks: core.BackgroundTaskConfig{ExecutorV2: true, CanaryUserIDs: []string{user.String()}}}
			spec := core.AgentTask{UserID: user, Title: "Voice AI market", Strategy: core.StrategyDirect, Handler: "background"}
			spec.ExecutorVersion = cfg.BackgroundTasks.ExecutorVersion(spec)
			created, err := store.Create(core.WithSoulID(ctx, uuid.New()), spec)
			if err != nil || created.ExecutorVersion != 2 {
				t.Fatal(created, err)
			}
			cfg.ApplyDefaults()
			deps := &core.Deps{Config: cfg, EnsureAutonomousHistory: func(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }}
			scheduler := agenttask.NewScheduler(store, nil, nil, core.NewToolRegistry(), nil, deps, nil, slog.New(slog.DiscardHandler))
			handler := &neverStopStub{plan: plan, executed: map[string]int{}}
			scheduler.SetGraphHandler(handler)
			var task core.AgentTask
			for i := 0; i < 5; i++ {
				if err := scheduler.Run(ctx); err != nil {
					t.Fatal(err)
				}
				scheduler.Wait()
				if task, err = store.Get(ctx, created.ID); err != nil {
					t.Fatal(err)
				}
				if task.Status == "done" || task.Status == "failed" {
					break
				}
			}
			if task.Status != "done" || task.Result == nil || !strings.HasPrefix(*task.Result, "# Market") {
				t.Fatalf("task stopped short of its report: %s %v", task.Status, task.Result)
			}
			gap := handler.reportIn["research"] + handler.reportIn["save"]
			if !strings.Contains(gap, "[unverified_step_result]") || !strings.Contains(gap, "source unavailable") {
				t.Fatalf("report did not receive the stuck branch and its gap: %q", gap)
			}
			if name == "unconfirmed_action" && handler.executed["save"] != 1 {
				t.Fatal("unconfirmed action was repeated", handler.executed)
			}
			var notice string
			if err := db.Get(&notice, `SELECT message_text FROM agent_task_notification_attempts WHERE task_id=$1`, created.ID); err != nil || notice != core.TaskReportNotification("Done: %s", task.Title, *task.Result, created.ID) {
				t.Fatalf("notice is not the ready message with report button: %q %v", notice, err)
			}
		})
	}
}
