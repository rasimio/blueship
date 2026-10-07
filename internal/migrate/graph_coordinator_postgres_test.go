package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/core"
)

func TestGraphCoordinatorRenewsThroughExecutionAndFinalizes(t *testing.T) {
	db := finalizationDB(t)
	for _, name := range []string{"026_agent_task_steps.sql", "027_agent_task_executor_version.sql", "028_agent_task_runs.sql"} {
		migration, err := migrations.ReadFile("sql/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := core.NewAgentTaskStore(db)
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	coordinator := agenttask.GraphCoordinator{Store: store, Lease: time.Second}
	claimed, err := coordinator.Run(ctx, id, func(ctx context.Context, task core.AgentTask) error {
		if err := store.SaveTaskSteps(ctx, id, *task.LastRunAt, []core.TaskStep{{ID: "read", Goal: "Read source", Acceptance: "Verified source", Kind: "read"}}); err != nil {
			return err
		}
		exec := agenttask.GraphExecutor{Store: store, ParallelReads: 2, Lease: time.Second}
		if err := exec.Run(ctx, id, func(ctx context.Context, step core.TaskStep, _ map[string]string, checkpoint func(context.Context, json.RawMessage) error) (agenttask.StepResult, error) {
			if err := checkpoint(ctx, json.RawMessage(`{"saved":true}`)); err != nil {
				return agenttask.StepResult{}, err
			}
			// Outlive both initial leases; only persisted renewal can allow commit.
			timer := time.NewTimer(1200 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return agenttask.StepResult{}, ctx.Err()
			}
			return agenttask.StepResult{Output: "Verified report", Accepted: true}, nil
		}); err != nil {
			return err
		}
		_, _, err := store.FinalizeTask(ctx, id, core.TaskFinalization{Outcome: "completed", Body: "Verified report"})
		return err
	})
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	task, err := store.Get(ctx, id)
	if err != nil || task.Status != "done" || task.Result == nil || *task.Result != "Verified report" {
		t.Fatal(task, err)
	}
	var renewed bool
	if err := db.Get(&renewed, `SELECT heartbeat_at>started_at+interval '500 milliseconds' FROM agent_task_runs WHERE task_id=$1`, id); err != nil || !renewed {
		t.Fatal("coordinator did not renew", renewed, err)
	}
	if again, err := coordinator.Run(ctx, id, func(context.Context, core.AgentTask) error { t.Fatal("terminal task replayed"); return nil }); err != nil || again {
		t.Fatal(again, err)
	}
	// Persisted cancellation is observed by the coordinator heartbeat and
	// propagates to the active handler, without requiring a process restart.
	cancelledID := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2 WHERE id=$1`, cancelledID); err != nil {
		t.Fatal(err)
	}
	claimed, err = coordinator.Run(ctx, cancelledID, func(workerCtx context.Context, _ core.AgentTask) error {
		if _, err := db.Exec(`UPDATE agent_tasks SET status='canceled' WHERE id=$1`, cancelledID); err != nil {
			return err
		}
		<-workerCtx.Done()
		return workerCtx.Err()
	})
	if !claimed || !errors.Is(err, context.Canceled) {
		t.Fatal("persisted cancellation did not stop worker", claimed, err)
	}

}
