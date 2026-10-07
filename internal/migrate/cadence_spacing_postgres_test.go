package migrate

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/core"
)

type cadenceStub struct {
	runs atomic.Int32
	sent func(run int32) bool
}

func (h *cadenceStub) Run(context.Context, core.AgentTask, core.AgentDeps) (core.IterationResult, error) {
	run := h.runs.Add(1)
	calls := []map[string]any{{"name": "browser_fetch"}}
	if h.sent(run) {
		calls = append(calls, map[string]any{"name": "message_send", "output": `{"sent":true}`})
	} else {
		calls = append(calls, map[string]any{"name": "message_send", "error": true, "output": "message is too long"})
	}
	raw, _ := json.Marshal(calls)
	return core.IterationResult{Progress: json.RawMessage(`{}`), ToolCallsJSON: raw}, nil
}

func (h *cadenceStub) DefaultTools() []string { return nil }

// A daily task delivers once per cadence. Before, every non-final iteration
// ran back-to-back, so each attempt sent another tale (15 in two hours,
// 2026-09-24); work that delivered nothing still continues within the run.
func TestCadenceTaskWaitsForCadenceAfterDelivering(t *testing.T) {
	db := finalizationDB(t)
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid DEFAULT gen_random_uuid(), ADD COLUMN cadence text, ADD COLUMN description text, ADD COLUMN acceptance_criteria text, ADD COLUMN delegate_to text, ADD COLUMN tools text[] NOT NULL DEFAULT '{}', ADD COLUMN use_agents text[] NOT NULL DEFAULT '{}', ADD COLUMN session_id text`); err != nil {
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
	ctx := core.WithSoulID(context.Background(), uuid.New())
	store := core.NewAgentTaskStore(db)
	cadence := "24h"
	task, err := store.Create(ctx, core.AgentTask{UserID: uuid.New(), Title: "Ежедневная сказка", Strategy: core.StrategyDirect, Cadence: &cadence, MaxIterations: 30, ExecutorVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Two research iterations deliver nothing, the third sends the tale.
	handler := &cadenceStub{sent: func(run int32) bool { return run >= 3 }}
	cfg := &core.Config{}
	cfg.ApplyDefaults()
	scheduler := agenttask.NewScheduler(store, nil, map[string]core.AgentHandler{core.StrategyDirect: handler}, core.NewToolRegistry(), nil, &core.Deps{Config: cfg}, nil, slog.New(slog.DiscardHandler))
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	if got := handler.runs.Load(); got != 3 {
		t.Fatalf("iterations in the first run = %d, want 3 (continue until the tale is sent, then stop)", got)
	}
	// The next tick falls inside the cadence: nothing runs, nothing is sent.
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	if got := handler.runs.Load(); got != 3 {
		t.Fatalf("a second tale was started within the cadence: %d iterations", got)
	}
	current, err := store.Get(ctx, task.ID)
	if err != nil || current.Status != "pending" || current.Iteration != 3 {
		t.Fatalf("task should wait for its next day: %+v %v", current, err)
	}
}
