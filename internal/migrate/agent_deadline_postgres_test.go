package migrate

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

func TestAgentTaskDeadlinePersistenceAndLateWorkerFence(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set BLUESHIP_TEST_POSTGRES_DSN to run PostgreSQL agent-task tests")
	}
	db := newAgentTaskSchema(t, dsn)
	_, err := db.Exec(`ALTER TABLE agent_tasks
		ADD COLUMN soul_id uuid, ADD COLUMN description text, ADD COLUMN acceptance_criteria text,
		ADD COLUMN delegate_to text, ADD COLUMN tools text[] NOT NULL DEFAULT '{}',
		ADD COLUMN use_agents text[] NOT NULL DEFAULT '{}', ADD COLUMN session_id text, ADD COLUMN cadence text;
		CREATE TABLE agent_task_notification_attempt_items (task_id uuid, input_id text, item_key text);`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := core.WithSoulID(context.Background(), uuid.New())
	store := core.NewAgentTaskStore(db)
	task, err := store.Create(ctx, core.AgentTask{UserID: uuid.New(), Title: "deadline regression", Strategy: core.StrategyDirect})
	if err != nil {
		t.Fatal(err)
	}
	if task.Deadline == nil || task.Deadline.Sub(task.CreatedAt) < 29*time.Minute || task.Deadline.Sub(task.CreatedAt) > 31*time.Minute {
		t.Fatalf("create did not persist the thirty-minute budget: %+v", task)
	}
	for _, status := range []string{"pending", "paused", "running"} {
		deadline := time.Now().Add(-time.Minute)
		if _, err := db.Exec(`UPDATE agent_tasks SET status = $2, deadline = NULL WHERE id = $1`, task.ID, status); err != nil {
			t.Fatal(err)
		}
		if err := store.SetTaskDeadline(ctx, task.ID, deadline); err != nil {
			t.Fatal(err)
		}
		if changed, err := store.ExpireTask(ctx, task.ID, time.Now()); err != nil || !changed {
			t.Fatal(changed, err)
		}
		// Writes from an iteration canceled by the wall deadline cannot resurrect it.
		if err := store.Complete(ctx, task.ID, "late report"); err == nil {
			t.Fatal("late completion reported success")
		}
		if err := store.PauseTask(ctx, task.ID, nil); err != nil {
			t.Fatal(err)
		}
		if err := store.SetPendingForNotificationRetry(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "failed" || got.CompletedAt == nil || got.ErrorMessage == nil || *got.ErrorMessage != core.TaskDeadlineExceeded {
			t.Fatal(got)
		}
		// A crash between expiration and journal reservation is recovered next tick.
		candidates, err := store.DeadlineTasks(ctx)
		if err != nil || len(candidates) != 1 {
			t.Fatal(candidates, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO agent_task_notification_attempt_items VALUES ($1, 'task_lifecycle', 'deadline')`, task.ID); err != nil {
		t.Fatal(err)
	}
	if candidates, err := store.DeadlineTasks(ctx); err != nil || len(candidates) != 0 {
		t.Fatal(candidates, err)
	}
	// Expired pending tasks are not claimable even if a worker had a stale snapshot.
	if _, err := db.Exec(`UPDATE agent_tasks SET status = 'pending' WHERE id = $1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.TrySetRunning(ctx, task.ID); err != nil || claimed {
		t.Fatal(claimed, err)
	}
}
