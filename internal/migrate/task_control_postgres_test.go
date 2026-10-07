package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
	"testing"
	"time"
)

func TestOwnedCancellationPreservesDraftAndFencesWorker(t *testing.T) {
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
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid, ADD COLUMN cadence text`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, soul := uuid.New(), uuid.New()
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2,strategy='direct',user_id=$2,soul_id=$3 WHERE id=$1`, id, user, soul); err != nil {
		t.Fatal(err)
	}
	store := core.NewAgentTaskStore(db)
	control := core.NewTaskController(db)
	run, err := store.ClaimGraphTask(ctx, id, time.Minute)
	if err != nil || run == nil {
		t.Fatal(run, err)
	}
	owner := core.WithTaskRunID(ctx, run.RunID)
	task, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTaskSteps(owner, id, *task.LastRunAt, []core.TaskStep{{ID: "read", Goal: "Read", Acceptance: "Source", Kind: "read"}, {ID: "report", Goal: "Report", Acceptance: "Complete", Kind: "finalize", Dependencies: []string{"read"}}}); err != nil {
		t.Fatal(err)
	}
	read, err := store.ClaimTaskStep(owner, id, "read", time.Minute)
	if err != nil || read == nil {
		t.Fatal(read, err)
	}
	if err := store.CompleteTaskStep(owner, id, "read", *read.RunID, "confirmed source"); err != nil {
		t.Fatal(err)
	}
	report, err := store.ClaimTaskStep(owner, id, "report", time.Minute)
	if err != nil || report == nil {
		t.Fatal(report, err)
	}
	for _, scope := range [][2]uuid.UUID{{uuid.New(), soul}, {user, uuid.New()}, {uuid.Nil, soul}} {
		if _, err := control.Cancel(ctx, scope[0], scope[1], id, "Cancelled"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("scope accepted", err)
		}
	}
	changed, err := control.Cancel(ctx, user, soul, id, "Cancelled before results")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	task, err = store.Get(ctx, id)
	if err != nil || task.Status != "canceled" || task.Result == nil || *task.Result != "Read\nconfirmed source" {
		t.Fatal(task, err)
	}
	if err := store.CompleteTaskStep(owner, id, "report", *report.RunID, "late report"); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatal("late worker wrote", err)
	}
	if changed, err := control.Cancel(ctx, user, soul, id, "second cancel"); err != nil || changed {
		t.Fatal(changed, err)
	}
	var n int
	if err := db.Get(&n, `SELECT count(*) FROM agent_task_artifacts WHERE task_id=$1`, id); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestOwnedTaskResolutionScopesPrefixAndRejectsAmbiguity(t *testing.T) {
	db := finalizationDB(t)
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, soul := uuid.New(), uuid.New()
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$3 WHERE id=$1`, id, user, soul); err != nil {
		t.Fatal(err)
	}
	store := core.NewAgentTaskStore(db)
	for _, scope := range [][2]uuid.UUID{{uuid.New(), soul}, {user, uuid.New()}, {uuid.Nil, soul}} {
		for _, raw := range []string{id.String(), id.String()[:8]} {
			if _, err := store.ResolveOwned(ctx, scope[0], scope[1], raw); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("foreign task resolved", err)
			}
		}
	}
	other := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	collision := uuid.MustParse(id.String()[:8] + other.String()[8:])
	if _, err := db.Exec(`UPDATE agent_tasks SET id=$2,user_id=$3,soul_id=$4 WHERE id=$1`, other, collision, uuid.New(), soul); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{id.String(), id.String()[:8]} {
		got, err := store.ResolveOwned(ctx, user, soul, raw)
		if err != nil || got.ID != id {
			t.Fatal("foreign prefix collision hid owned task", got, err)
		}
	}
	for _, raw := range []string{"", "%", id.String()[:7], collision.String()} {
		if _, err := store.ResolveOwned(ctx, user, soul, raw); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("invalid or foreign identifier resolved", raw, err)
		}
	}
	if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2 WHERE id=$1`, collision, user); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveOwned(ctx, user, soul, id.String()[:8]); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("ambiguous prefix resolved", err)
	}
	if got, err := store.ResolveOwned(ctx, user, soul, id.String()); err != nil || got.ID != id {
		t.Fatal("full identifier became ambiguous", got, err)
	}
}
