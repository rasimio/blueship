package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

func TestTaskCheckpointFencesRecoveryAndCancellation(t *testing.T) {
	dsn := os.Getenv("BLUESHIP_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BLUESHIP_TEST_POSTGRES_DSN")
	}
	db := newAgentTaskSchema(t, dsn)
	migration, err := migrations.ReadFile("sql/024_agent_task_checkpoints.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	store := core.NewAgentTaskStore(db)
	ctx := context.Background()
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{"evidence":["preserved"]}`))
	if ok, err := store.TrySetRunning(ctx, id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	claimed, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	cp := core.TaskCheckpoint{Iteration: 1, SessionID: "worker-session", Phase: "tool_started", Progress: json.RawMessage(`{"session_id":"worker-session"}`), PendingTool: json.RawMessage(`{"name":"external_write","id":"operation-1"}`)}
	if err := store.CheckpointTask(ctx, id, *claimed.LastRunAt, cp); err != nil {
		t.Fatal(err)
	}
	saved, err := store.TaskCheckpoint(ctx, id)
	if err != nil || saved == nil || saved.SessionID != cp.SessionID || string(saved.PendingTool) != `{"id": "operation-1", "name": "external_write"}` {
		t.Fatalf("checkpoint=%+v err=%v", saved, err)
	}
	updated, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var progress map[string]any
	if err := json.Unmarshal(updated.Progress, &progress); err != nil {
		t.Fatal(err)
	}
	if progress["evidence"] == nil || progress["session_id"] != "worker-session" {
		t.Fatalf("checkpoint lost progress: %s", updated.Progress)
	}
	// Recovery changes the claim. A late old worker cannot save or clear the
	// new worker's unresolved operation, even though status is running again.
	nextClaim := claimed.LastRunAt.Add(time.Second)
	if _, err := db.Exec(`UPDATE agent_tasks SET last_run_at=$2 WHERE id=$1`, id, nextClaim); err != nil {
		t.Fatal(err)
	}
	cp.Output = "late stale output"
	if err := store.CheckpointTask(ctx, id, *claimed.LastRunAt, cp); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatalf("stale claim: %v", err)
	}
	cp.Output, cp.Phase, cp.PendingTool = "confirmed partial result", "tool_completed", nil
	if err := store.CheckpointTask(ctx, id, nextClaim, cp); err != nil {
		t.Fatal(err)
	}
	// Session-start checkpoints on a subsequent attempt retain the best draft.
	cp.Output = ""
	if err := store.CheckpointTask(ctx, id, nextClaim, cp); err != nil {
		t.Fatal(err)
	}
	saved, err = store.TaskCheckpoint(ctx, id)
	if err != nil || saved.Output != "confirmed partial result" || string(saved.PendingTool) != "null" {
		t.Fatalf("completed checkpoint=%+v err=%v", saved, err)
	}
	for _, terminal := range []string{"done", "failed", "canceled"} {
		setTaskStatus(t, ctx, db, id, terminal)
		if err := store.CheckpointTask(ctx, id, nextClaim, cp); !errors.Is(err, core.ErrTaskClaimLost) {
			t.Fatalf("%s allowed late checkpoint: %v", terminal, err)
		}
	}
}
