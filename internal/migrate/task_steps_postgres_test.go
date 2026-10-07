package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

func TestTaskStepsDependenciesLeaseRecoveryAndFencing(t *testing.T) {
	db := finalizationDB(t)
	data, err := migrations.ReadFile("sql/026_agent_task_steps.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store := core.NewAgentTaskStore(db)
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if ok, err := store.TrySetRunning(ctx, id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	task, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	steps := []core.TaskStep{
		{ID: "read", Goal: "Read source", Acceptance: "Source fetched", Kind: "read", MaxAttempts: 2},
		{ID: "write", Goal: "Save document", Acceptance: "Document receipt", Kind: "action", Dependencies: []string{"read"}},
		{ID: "other", Goal: "Read second source", Acceptance: "Source fetched", Kind: "read"},
	}
	if err := store.SaveTaskSteps(ctx, id, *task.LastRunAt, steps); err != nil {
		t.Fatal(err)
	}
	if step, err := store.ClaimTaskStep(ctx, id, "write", time.Minute); err != nil || step != nil {
		t.Fatal("dependency released early", step, err)
	}
	read, err := store.ClaimTaskStep(ctx, id, "read", time.Minute)
	if err != nil || read == nil {
		t.Fatal(read, err)
	}
	other, err := store.ClaimTaskStep(ctx, id, "other", time.Minute)
	if err != nil || other == nil {
		t.Fatal("independent step cannot run", other, err)
	}
	if err := store.CheckpointTaskStep(ctx, id, "read", *read.RunID, time.Minute, json.RawMessage(`{"url":"source"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE agent_task_steps SET lease_until=now()-interval '1 second' WHERE task_id=$1 AND step_id='read'`, id); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteTaskStep(ctx, id, "read", *read.RunID, "late"); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatal("expired worker wrote", err)
	}
	if err := store.RecoverTaskSteps(ctx, id); err != nil {
		t.Fatal(err)
	}
	replacement, err := store.ClaimTaskStep(ctx, id, "read", time.Minute)
	if err != nil || replacement == nil || *replacement.RunID == *read.RunID || replacement.Attempts != 2 {
		t.Fatal(replacement, err)
	}
	if err := store.CompleteTaskStep(ctx, id, "read", *read.RunID, "stale"); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatal(err)
	}
	if err := store.CompleteTaskStep(ctx, id, "read", *replacement.RunID, "Confirmed source"); err != nil {
		t.Fatal(err)
	}
	// An action cannot overlap an unrelated read owned by another worker.
	if action, err := store.ClaimTaskStep(ctx, id, "write", time.Minute); err != nil || action != nil {
		t.Fatal("action overlapped read", action, err)
	}
	// Exhaust this read so recovery makes the action exclusive.
	if _, err := db.Exec(`UPDATE agent_task_steps SET lease_until=now()-interval '1 second',max_attempts=attempts WHERE task_id=$1 AND step_id='other'`, id); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverTaskSteps(ctx, id); err != nil {
		t.Fatal(err)
	}
	action, err := store.ClaimTaskStep(ctx, id, "write", time.Minute)
	if err != nil || action == nil {
		t.Fatal(action, err)
	}
	if _, err := db.Exec(`UPDATE agent_task_steps SET lease_until=now()-interval '1 second' WHERE task_id=$1 AND step_id='write'`, id); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverTaskSteps(ctx, id); err != nil {
		t.Fatal(err)
	}
	if retry, err := store.ClaimTaskStep(ctx, id, "write", time.Minute); err != nil || retry != nil {
		t.Fatal("ambiguous action replayed", retry, err)
	}
	var state string
	if err := db.Get(&state, `SELECT status FROM agent_task_steps WHERE task_id=$1 AND step_id='write'`, id); err != nil || state != "reconciliation" {
		t.Fatal(state, err)
	}

	// Exhausted reads stop instead of cycling through recovery forever.
	if _, err := db.Exec(`UPDATE agent_task_steps SET lease_until=now()-interval '1 second',max_attempts=attempts WHERE task_id=$1 AND step_id='other'`, id); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverTaskSteps(ctx, id); err != nil {
		t.Fatal(err)
	}
	if retry, err := store.ClaimTaskStep(ctx, id, "other", time.Minute); err != nil || retry != nil {
		t.Fatal("exhausted read replayed", retry, err)
	}
	if err := db.Get(&state, `SELECT status FROM agent_task_steps WHERE task_id=$1 AND step_id='other'`, id); err != nil || state != "blocked" {
		t.Fatal(state, err)
	}
	if _, err := db.Exec(`UPDATE agent_tasks SET status='done' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteTaskStep(ctx, id, "other", *other.RunID, "late"); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatal("terminal parent accepted write", err)
	}
	if err := store.CheckpointTaskStep(ctx, id, "other", uuid.New(), time.Minute, json.RawMessage(`{}`)); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatal(err)
	}
}

func TestTaskStatusSeparatesHeartbeatsFromSavedProgress(t *testing.T) {
	db := finalizationDB(t)
	ctx := context.Background()
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid, ADD COLUMN cadence text`); err != nil {
		t.Fatal(err)
	}
	owner, soul := uuid.New(), uuid.New()
	store := core.NewAgentTaskStore(db)
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$3,strategy='direct' WHERE id=$1`, id, owner, soul); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.TrySetRunning(ctx, id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	task, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTaskSteps(ctx, id, *task.LastRunAt, []core.TaskStep{{ID: "read", Kind: "read", Goal: "Read source", Acceptance: "Source saved"}}); err != nil {
		t.Fatal(err)
	}
	step, err := store.ClaimTaskStep(ctx, id, "read", time.Minute)
	if err != nil || step == nil {
		t.Fatal(step, err)
	}
	checkpoint := json.RawMessage(`{"output":"saved source"}`)
	if err := store.CheckpointTaskStep(ctx, id, "read", *step.RunID, time.Minute, checkpoint); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-8 * time.Minute).Truncate(time.Microsecond)
	if _, err := db.Exec(`UPDATE agent_task_steps SET progress_at=$2 WHERE task_id=$1`, id, old); err != nil {
		t.Fatal(err)
	}
	for _, cp := range []json.RawMessage{nil, checkpoint} {
		if err := store.CheckpointTaskStep(ctx, id, "read", *step.RunID, time.Minute, cp); err != nil {
			t.Fatal(err)
		}
		var progress, heartbeat time.Time
		if err := db.QueryRow(`SELECT progress_at,heartbeat_at FROM agent_task_steps WHERE task_id=$1`, id).Scan(&progress, &heartbeat); err != nil {
			t.Fatal(err)
		}
		if !progress.Equal(old) || !heartbeat.After(old) {
			t.Fatal("heartbeat falsely reported progress", progress, heartbeat)
		}
	}
	heartbeat := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.Exec(`INSERT INTO agent_task_runs(task_id,run_id,lease_until,heartbeat_at) VALUES($1,$2,now()+interval '1 minute',$3)`, id, uuid.New(), heartbeat); err != nil {
		t.Fatal(err)
	}
	reader := core.NewTaskStatusReader(db)
	rows, err := reader.Read(ctx, owner, soul, id.String())
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if rows[0].LastProgressAt == nil || !rows[0].LastProgressAt.Equal(old) || rows[0].WorkerHeartbeatAt == nil || !rows[0].WorkerHeartbeatAt.Equal(heartbeat) {
		t.Fatal("fresh heartbeat hid stale progress", rows[0])
	}
	if err := store.CheckpointTaskStep(ctx, id, "read", *step.RunID, time.Minute, json.RawMessage(`{"output":"additional source"}`)); err != nil {
		t.Fatal(err)
	}
	rows, err = reader.Read(ctx, owner, soul, id.String())
	if err != nil || len(rows) != 1 || rows[0].LastProgressAt == nil || !rows[0].LastProgressAt.After(old) {
		t.Fatal(rows, err)
	}
	if err := store.CompleteTaskStep(ctx, id, "read", *step.RunID, "Verified source"); err != nil {
		t.Fatal(err)
	}
	var completed, progress time.Time
	if err := db.QueryRow(`SELECT completed_at,progress_at FROM agent_task_steps WHERE task_id=$1`, id).Scan(&completed, &progress); err != nil {
		t.Fatal(err)
	}
	if progress.Before(completed) {
		t.Fatal("completion did not record advancement", completed, progress)
	}
}
