package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

func TestGraphCoordinatorRecoveryFencesOldStepsAndFinalization(t *testing.T) {
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
	ctx := context.Background()
	store := core.NewAgentTaskStore(db)
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimGraphTask(ctx, id, time.Minute)
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	owner := core.WithTaskRunID(ctx, first.RunID)
	if duplicate, err := store.ClaimGraphTask(ctx, id, time.Minute); err != nil || duplicate != nil {
		t.Fatal("double coordinator", duplicate, err)
	}
	task, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTaskSteps(owner, id, *task.LastRunAt, []core.TaskStep{{ID: "read", Goal: "Read source", Acceptance: "Confirmed source", Kind: "read"}}); err != nil {
		t.Fatal(err)
	}
	read, err := store.ClaimTaskStep(owner, id, "read", time.Minute)
	if err != nil || read == nil {
		t.Fatal(read, err)
	}
	if err := store.YieldGraphTask(owner, id, time.Now()); err == nil {
		t.Fatal("yielded with running worker")
	}
	if err := store.RenewGraphTask(owner, id, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE agent_task_runs SET lease_until=now()-interval '1 second' WHERE task_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := store.RenewGraphTask(owner, id, time.Minute); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatal("expired owner revived", err)
	}
	second, err := store.ClaimGraphTask(ctx, id, time.Minute)
	if err != nil || second == nil || second.RunID == first.RunID {
		t.Fatal(second, err)
	}
	if err := store.CompleteTaskStep(owner, id, "read", *read.RunID, "late result"); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatal("old coordinator wrote", err)
	}
	if _, _, err := store.FinalizeTask(owner, id, core.TaskFinalization{Outcome: "completed", Body: "stale report"}); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatal("old coordinator finalized", err)
	}
	replacement := core.WithTaskRunID(ctx, second.RunID)
	read, err = store.ClaimTaskStep(replacement, id, "read", time.Minute)
	if err != nil || read == nil {
		t.Fatal(read, err)
	}
	if err := store.CompleteTaskStep(ctx, id, "read", *read.RunID, "no owner"); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatal("missing coordinator accepted", err)
	}
	if err := store.CompleteTaskStep(replacement, id, "read", *read.RunID, "Confirmed source"); err != nil {
		t.Fatal(err)
	}
	if err := store.YieldGraphTask(replacement, id, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.RunnableGraphTasks(ctx, 10)
	if err != nil || len(tasks) != 0 {
		t.Fatal("backoff ignored", tasks, err)
	}
	if third, err := store.ClaimGraphTask(ctx, id, time.Minute); err != nil || third != nil {
		t.Fatal("claim ignored backoff", third, err)
	}
	if _, err := db.Exec(`UPDATE agent_task_runs SET next_attempt_at=now()-interval '1 second' WHERE task_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	tasks, err = store.RunnableGraphTasks(ctx, 10)
	if err != nil || len(tasks) != 1 || tasks[0].ID != id {
		t.Fatal(tasks, err)
	}
	third, err := store.ClaimGraphTask(ctx, id, time.Minute)
	if err != nil || third == nil {
		t.Fatal(third, err)
	}
	artifact, changed, err := store.FinalizeTask(core.WithTaskRunID(ctx, third.RunID), id, core.TaskFinalization{Outcome: "partial", Reason: "source unavailable", EmptyBody: "No results", Notify: "Partial report saved"})
	if err != nil || !changed || artifact.Body != "Read source\nConfirmed source" {
		t.Fatal("confirmed step result lost", artifact, changed, err)
	}

}

func TestGraphRecoveryDistinguishesSavedCandidatesFromUncertainActions(t *testing.T) {
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
	store := core.NewAgentTaskStore(db)
	for _, mode := range []string{"coordinator", "step", "failure"} {
		for _, tc := range []struct {
			name, kind, checkpoint, want string
			tools                        []string
		}{
			{name: "review candidate", kind: "action", checkpoint: `{"candidate_ready":true,"phase":"verification","output":"saved document"}`, want: "pending"},
			{name: "verified candidate", kind: "action", checkpoint: `{"candidate_ready":true,"phase":"verified","output":"saved document","pending_tool":null}`, want: "pending"},
			{name: "unresolved operation", kind: "action", checkpoint: `{"candidate_ready":true,"phase":"verification","output":"draft","pending_tool":{"name":"send"}}`, want: "reconciliation"},
			{name: "missing checkpoint", kind: "action", checkpoint: `{}`, want: "reconciliation"},
			{name: "empty candidate", kind: "action", checkpoint: `{"candidate_ready":true,"phase":"verification","output":"   "}`, want: "reconciliation"},
			{name: "missing candidate", kind: "action", checkpoint: `{"candidate_ready":true,"phase":"verification"}`, want: "reconciliation"},
			{name: "rejected action", kind: "action", checkpoint: `{"candidate_ready":true,"phase":"rejected","output":"saved document"}`, want: "reconciliation"},
			{name: "pure synthesis", kind: "finalize", checkpoint: `{}`, want: "pending"},
			{name: "artifact creation", kind: "finalize", tools: []string{"create_file"}, checkpoint: `{}`, want: "reconciliation"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
				if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2 WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
				run, err := store.ClaimGraphTask(ctx, id, time.Minute)
				if err != nil || run == nil {
					t.Fatal(run, err)
				}
				owner := core.WithTaskRunID(ctx, run.RunID)
				task, err := store.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SaveTaskSteps(owner, id, *task.LastRunAt, []core.TaskStep{{ID: "work", Goal: "Do work", Acceptance: "Confirmed result", Kind: tc.kind, Tools: tc.tools}}); err != nil {
					t.Fatal(err)
				}
				step, err := store.ClaimTaskStep(owner, id, "work", time.Minute)
				if err != nil || step == nil {
					t.Fatal(step, err)
				}
				if err := store.CheckpointTaskStep(owner, id, "work", *step.RunID, time.Minute, json.RawMessage(tc.checkpoint)); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "coordinator":
					if _, err := db.Exec(`UPDATE agent_task_runs SET lease_until=now()-interval '1 second' WHERE task_id=$1`, id); err != nil {
						t.Fatal(err)
					}
					replacement, err := store.ClaimGraphTask(ctx, id, time.Minute)
					if err != nil || replacement == nil {
						t.Fatal(replacement, err)
					}
				case "step":
					if _, err := db.Exec(`UPDATE agent_task_steps SET lease_until=now()-interval '1 second' WHERE task_id=$1`, id); err != nil {
						t.Fatal(err)
					}
					if err := store.RecoverTaskSteps(owner, id); err != nil {
						t.Fatal(err)
					}
				case "failure":
					if err := store.FailTaskStep(owner, id, "work", *step.RunID, "review unavailable", true); err != nil {
						t.Fatal(err)
					}
				}
				steps, err := store.TaskSteps(ctx, id)
				if err != nil || len(steps) != 1 {
					t.Fatal(steps, err)
				}
				if steps[0].Status != tc.want {
					t.Fatalf("status=%s want=%s", steps[0].Status, tc.want)
				}
				var before, after any
				if err := json.Unmarshal([]byte(tc.checkpoint), &before); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(steps[0].Checkpoint, &after); err != nil {
					t.Fatal(err)
				}
				a, _ := json.Marshal(before)
				b, _ := json.Marshal(after)
				if string(a) != string(b) {
					t.Fatal("recovery changed saved candidate", string(b))
				}
			})
		}
	}
}

func TestGraphNextWakeUsesRetryLeaseAndDeadline(t *testing.T) {
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
	store := core.NewAgentTaskStore(db)
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if at, err := store.NextGraphWake(ctx); err != nil || !at.IsZero() {
		t.Fatal("runnable work created a timer loop", at, err)
	}
	run, err := store.ClaimGraphTask(ctx, id, time.Minute)
	if err != nil || run == nil {
		t.Fatal(run, err)
	}
	assertNext := func(want time.Time) {
		t.Helper()
		at, err := store.NextGraphWake(ctx)
		if err != nil || at.Sub(want).Abs() > time.Millisecond {
			t.Fatal(at, want, err)
		}
	}
	assertNext(run.LeaseUntil)
	retry := time.Now().Add(5 * time.Second).Truncate(time.Microsecond)
	if err := store.YieldGraphTask(core.WithTaskRunID(ctx, run.RunID), id, retry); err != nil {
		t.Fatal(err)
	}
	assertNext(retry)
	deadline := time.Now().Add(time.Second).Truncate(time.Microsecond)
	if _, err := db.Exec(`UPDATE agent_tasks SET deadline=$2 WHERE id=$1`, id, deadline); err != nil {
		t.Fatal(err)
	}
	assertNext(deadline)
	if _, err := db.Exec(`UPDATE agent_tasks SET status='done' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if at, err := store.NextGraphWake(ctx); err != nil || !at.IsZero() {
		t.Fatal("terminal task retained timer", at, err)
	}
	if _, err := db.Exec(`UPDATE agent_tasks SET status='pending',executor_version=1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if at, err := store.NextGraphWake(ctx); err != nil || !at.IsZero() {
		t.Fatal("legacy task changed v2 wake time", at, err)
	}
}
