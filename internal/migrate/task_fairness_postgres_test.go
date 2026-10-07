package migrate

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

func TestGraphAdmissionIsFairAndOwnerCapIsAtomic(t *testing.T) {
	db := finalizationDB(t)
	for _, name := range []string{"026_agent_task_steps.sql", "027_agent_task_executor_version.sql", "028_agent_task_runs.sql"} {
		data, err := migrations.ReadFile("sql/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	owner, other := uuid.New(), uuid.New()
	ids := []uuid.UUID{}
	for i := 0; i < 9; i++ {
		id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
		ids = append(ids, id)
		user := owner
		if i == 8 {
			user = other
		}
		if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,executor_version=2,strategy='direct',created_at=now()+$3*interval '1 second' WHERE id=$1`, id, user, i); err != nil {
			t.Fatal(err)
		}
	}
	store := core.NewAgentTaskStore(db)
	rows, err := store.RunnableGraphTasks(ctx, 16)
	if err != nil || len(rows) != 3 || rows[0].UserID != owner || rows[1].UserID != other {
		t.Fatal("large old backlog hid other owner", rows, err)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, id := range ids[:2] {
		if _, err := db.Exec(`UPDATE agent_tasks SET config=jsonb_build_object('start_at',$2::text) WHERE id=$1`, id, future); err != nil {
			t.Fatal(err)
		}
	}
	rows, err = store.RunnableGraphTasks(ctx, 16)
	if err != nil || len(rows) != 3 || rows[0].ID != ids[2] {
		t.Fatal("future head tasks starved ready work", rows, err)
	}
	if run, err := store.ClaimGraphTask(ctx, ids[0], time.Minute); err != nil || run != nil {
		t.Fatal("future task claimed early", run, err)
	}
	wake, err := store.NextGraphWake(ctx)
	if err != nil || time.Until(wake) < 59*time.Minute || time.Until(wake) > 61*time.Minute {
		t.Fatal("future start not scheduled", wake, err)
	}
	for i, invalid := range []string{"tomorrow", "2026-02-31T12:00:00Z"} {
		if _, err := db.Exec(`UPDATE agent_tasks SET config=jsonb_build_object('start_at',$2::text) WHERE id=$1`, ids[i], invalid); err != nil {
			t.Fatal(err)
		}
	}
	rows, err = store.RunnableGraphTasks(ctx, 16)
	if err != nil || len(rows) != 3 || rows[0].ID != ids[0] {
		t.Fatal("malformed date poisoned queue", rows, err)
	}
	var wg sync.WaitGroup
	runs := make(chan *core.TaskRun, 8)
	for _, id := range ids[:8] {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			run, err := store.ClaimGraphTask(ctx, id, time.Minute)
			if err != nil {
				t.Error(err)
				return
			}
			if run != nil {
				runs <- run
			}
		}(id)
	}
	wg.Wait()
	close(runs)
	var admitted []*core.TaskRun
	for run := range runs {
		admitted = append(admitted, run)
	}
	if len(admitted) != core.MaxConcurrentGraphTasksPerUser {
		t.Fatal("concurrent claims exceeded owner limit", len(admitted))
	}
	rows, err = store.RunnableGraphTasks(ctx, 16)
	if err != nil || len(rows) != 1 || rows[0].UserID != other {
		t.Fatal("capped owner remained eligible", rows, err)
	}
	first := admitted[0]
	if err := store.YieldGraphTask(core.WithTaskRunID(ctx, first.RunID), first.TaskID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err = store.RunnableGraphTasks(ctx, 16)
	if err != nil || len(rows) != 2 || rows[0].UserID != other || rows[1].UserID != owner || rows[1].ID == first.TaskID {
		t.Fatal("yield did not release admission or backoff ignored", rows, err)
	}
}
