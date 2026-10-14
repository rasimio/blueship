package migrate

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/core"
)

type recurringStub struct{ runs atomic.Int32 }

func (h *recurringStub) Run(context.Context, core.AgentTask, core.AgentDeps) (core.IterationResult, error) {
	h.runs.Add(1)
	return core.IterationResult{Progress: json.RawMessage(`{}`)}, nil
}

func (h *recurringStub) DefaultTools() []string { return nil }

type dueEarlyHost struct{ due map[uuid.UUID]bool }

func (h *dueEarlyHost) NextWake(context.Context, time.Time) (time.Time, error) {
	return time.Time{}, nil
}

func (h *dueEarlyHost) DueEarly(_ context.Context, _ time.Time, tasks []core.AgentTask) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	for _, task := range tasks {
		out[task.ID] = h.due[task.ID]
	}
	return out, nil
}

// A reminder set for 19:42 under a 5-minute heartbeat that ran at 19:39 used
// to wait for the 19:44 run (2026-10-14: due 19:42, delivered 19:44:06). The
// host can now bring that one run forward; without it the schedule holds.
func TestRecurringTaskRunsEarlyWhenHostWorkComesDue(t *testing.T) {
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
	schedule := "5m"
	task, err := store.Create(ctx, core.AgentTask{UserID: uuid.New(), Title: "Heartbeat", Strategy: core.StrategyRecurring, Handler: "heartbeat", Schedule: &schedule, MaxIterations: 1000, ExecutorVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	handler := &recurringStub{}
	cfg := &core.Config{}
	cfg.ApplyDefaults()
	deps := &core.Deps{Config: cfg}
	scheduler := agenttask.NewScheduler(store, map[string]core.AgentHandler{"heartbeat": handler}, nil, core.NewToolRegistry(), nil, deps, nil, slog.New(slog.DiscardHandler))
	tick := func() int32 {
		t.Helper()
		if err := scheduler.Run(ctx); err != nil {
			t.Fatal(err)
		}
		scheduler.Wait()
		return handler.runs.Load()
	}

	if got := tick(); got != 1 {
		t.Fatalf("first tick ran the heartbeat %d times, want 1", got)
	}
	if got := tick(); got != 1 {
		t.Fatalf("the heartbeat ran again inside its schedule with nothing due: %d runs", got)
	}
	deps.RecurringTaskWake = &dueEarlyHost{due: map[uuid.UUID]bool{}}
	if got := tick(); got != 1 {
		t.Fatalf("the host said nothing is due, yet the heartbeat ran: %d runs", got)
	}
	deps.RecurringTaskWake = &dueEarlyHost{due: map[uuid.UUID]bool{task.ID: true}}
	if got := tick(); got != 2 {
		t.Fatalf("a reminder came due, but the heartbeat waited for its schedule: %d runs", got)
	}
}
