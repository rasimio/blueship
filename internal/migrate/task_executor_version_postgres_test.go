package migrate

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

func TestLegacyMaintenanceDoesNotTouchV2Tasks(t *testing.T) {
	db := finalizationDB(t)
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid DEFAULT gen_random_uuid(), ADD COLUMN cadence text`); err != nil {
		t.Fatal(err)
	}
	migration, err := migrations.ReadFile("sql/027_agent_task_executor_version.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store := core.NewAgentTaskStore(db)
	legacy := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	modern := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2 WHERE id=$1`, modern); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.PendingTasks(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].ID != legacy {
		t.Fatal(tasks, err)
	}
	if ok, err := store.TrySetRunning(ctx, modern); err != nil || ok {
		t.Fatal("legacy claimed v2", ok, err)
	}
	if _, err := db.Exec(`UPDATE agent_tasks SET status='running',last_run_at=now()-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ResetStale(ctx, time.Minute); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	task, err := store.Get(ctx, modern)
	if err != nil || task.Status != "running" {
		t.Fatal(task, err)
	}
	if _, err := db.Exec(`UPDATE agent_tasks SET status='paused',last_run_at=now()-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if n, err := store.WakeStalePaused(ctx, time.Minute); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err := db.Exec(`UPDATE agent_tasks SET status='pending',iteration=max_iterations`); err != nil {
		t.Fatal(err)
	}
	exhausted := store.CompleteExhausted(ctx)
	if len(exhausted) != 1 || exhausted[0].ID != legacy {
		t.Fatal(exhausted)
	}
	task, err = store.Get(ctx, modern)
	if err != nil || task.Status != "pending" {
		t.Fatal(task, err)
	}
	if _, err := db.Exec(`UPDATE agent_tasks SET status='pending',deadline=now()-interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	tasks, err = store.DeadlineTasks(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].ID != legacy {
		t.Fatal(tasks, err)
	}
	if changed, err := store.ExpireTask(ctx, modern, time.Now()); err != nil || changed {
		t.Fatal("legacy expiry changed v2", changed, err)
	}
}
