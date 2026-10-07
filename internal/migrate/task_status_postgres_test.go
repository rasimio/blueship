package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/jmoiron/sqlx"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

func TestTaskStatusRequiresOwnerAndSoulForListsAndIDs(t *testing.T) {
	db := finalizationDB(t)
	ctx := context.Background()
	migration, err := migrations.ReadFile("sql/026_agent_task_steps.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid, ADD COLUMN cadence text`); err != nil {
		t.Fatal(err)
	}
	user, soul := uuid.New(), uuid.New()
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{"phase":"finalizing"}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$3,strategy='direct',result='private report' WHERE id=$1`, id, user, soul); err != nil {
		t.Fatal(err)
	}
	reader := core.NewTaskStatusReader(db)
	for _, scope := range [][2]uuid.UUID{{uuid.New(), soul}, {user, uuid.New()}, {uuid.Nil, soul}, {user, uuid.Nil}} {
		for _, query := range []string{id.String(), id.String()[:8]} {
			if rows, err := reader.Read(ctx, scope[0], scope[1], query); !errors.Is(err, sql.ErrNoRows) || len(rows) != 0 {
				t.Fatalf("scope leak: %+v %v", rows, err)
			}
		}
		if rows, err := reader.Read(ctx, scope[0], scope[1], ""); len(rows) != 0 || (err != nil && !errors.Is(err, sql.ErrNoRows)) {
			t.Fatalf("list scope leak: %+v %v", rows, err)
		}
	}
	rows, err := reader.Read(ctx, user, soul, "")
	if err != nil || len(rows) != 1 || rows[0].Stage != "finalizing" || rows[0].Result != nil || rows[0].ProgressPercent != nil || rows[0].ETASeconds != nil {
		t.Fatalf("list: %+v %v", rows, err)
	}

	if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,status,result) VALUES ($1,'a','Read','Verified','read','done','evidence'),($1,'b','Report','Verified','finalize','pending','')`, id); err != nil {
		t.Fatal(err)
	}
	rows, err = reader.Read(ctx, user, soul, "")
	if err != nil || len(rows) != 1 || rows[0].ProgressPercent == nil || *rows[0].ProgressPercent != 50 || rows[0].CompletedSteps != 1 || rows[0].TotalSteps != 2 {
		t.Fatalf("confirmed progress: %+v %v", rows, err)
	}

	if _, err := db.Exec(`UPDATE agent_tasks SET status='running',progress='{}' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ query, stage string }{
		{`UPDATE agent_task_steps SET status='running',checkpoint='{"phase":"executing","private":"not public"}' WHERE task_id=$1 AND step_id='b'`, "finalizing"},
		{`UPDATE agent_task_steps SET checkpoint='{"phase":"verification","private":"not public"}' WHERE task_id=$1 AND step_id='b'`, "verifying"},
		{`UPDATE agent_task_steps SET status='running',checkpoint='{"phase":"executing"}' WHERE task_id=$1 AND step_id='a'`, "running"},
	} {
		if _, err := db.Exec(tc.query, id); err != nil {
			t.Fatal(err)
		}
		rows, err = reader.Read(ctx, user, soul, "")
		if err != nil || len(rows) != 1 || rows[0].Stage != tc.stage || rows[0].Result != nil {
			t.Fatalf("wrong active checkpoint projection: %+v %v", rows, err)
		}
	}
	if _, err := db.Exec(`UPDATE agent_task_steps SET status='done',result='report' WHERE task_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	rows, err = reader.Read(ctx, user, soul, "")
	if err != nil || len(rows) != 1 || rows[0].ProgressPercent == nil || *rows[0].ProgressPercent != 99 {
		t.Fatalf("premature completion: %+v %v", rows, err)
	}

	for _, tc := range []struct{ progress, stage string }{
		{`{"phase":"finalizing","final_validation":{"repair_pending":false}}`, "verifying"},
		{`{"phase":"finalizing","final_validation":{"repair_pending":true}}`, "finalizing"},
	} {
		if _, err := db.Exec(`UPDATE agent_tasks SET progress=$2::jsonb WHERE id=$1`, id, tc.progress); err != nil {
			t.Fatal(err)
		}
		rows, err = reader.Read(ctx, user, soul, "")
		if err != nil || len(rows) != 1 || rows[0].Stage != tc.stage || rows[0].ProgressPercent == nil || *rows[0].ProgressPercent != 99 {
			t.Fatalf("final verification/repair projection: %+v %v", rows, err)
		}
	}
	if _, err := db.Exec(`UPDATE agent_tasks SET status='done',progress='{"result_outcome":"completed"}' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	rows, err = reader.Read(ctx, user, soul, id.String()[:8])
	if err != nil || len(rows) != 1 || rows[0].Result == nil || *rows[0].Result != "private report" || rows[0].ProgressPercent == nil || *rows[0].ProgressPercent != 100 {
		t.Fatalf("detail: %+v %v", rows, err)
	}
}

func TestTaskStatusETAUsesPooledHistoryAndDeadline(t *testing.T) {
	db := finalizationDB(t)
	ctx := context.Background()
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid, ADD COLUMN cadence text`); err != nil {
		t.Fatal(err)
	}
	user, soul := uuid.New(), uuid.New()
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$3,strategy='direct',status='running',deadline=now()+interval '20 minutes' WHERE id=$1`, id, user, soul); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,tools) VALUES($1,'read','Read','Verified','read','{browser_fetch}')`, id); err != nil {
		t.Fatal(err)
	}
	reader := core.NewTaskStatusReader(db)
	rows, err := reader.Read(ctx, user, soul, id.String())
	if err != nil || len(rows) != 1 || rows[0].ETASeconds != nil || rows[0].ETAUpperSeconds == nil || *rows[0].ETAUpperSeconds < 1190 || *rows[0].ETAUpperSeconds > 1200 {
		t.Fatalf("no history must fall back to the deadline bound: %+v %v", rows, err)
	}
	// Other owners' first-attempt durations of the same step kind count, even
	// with a different tool list; timing carries no task content.
	for i := 0; i < 5; i++ {
		history := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
		if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$3,strategy='direct',status='done' WHERE id=$1`, history, uuid.New(), uuid.New()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,tools,status,attempts,started_at,completed_at) VALUES($1,'read','Read','Verified','read','{web_search}','done',1,now()-interval '60 seconds',now())`, history); err != nil {
			t.Fatal(err)
		}
	}
	rows, err = reader.Read(ctx, user, soul, id.String())
	if err != nil || len(rows) != 1 || rows[0].ETASeconds == nil || *rows[0].ETASeconds != 60 || rows[0].ETAUpperSeconds == nil || *rows[0].ETAUpperSeconds != 60 {
		t.Fatalf("pooled history ignored: %+v %v", rows, err)
	}
	if _, err := db.Exec(`UPDATE agent_task_steps SET attempts=2 WHERE task_id<>$1`, id); err != nil {
		t.Fatal(err)
	}
	rows, err = reader.Read(ctx, user, soul, id.String())
	if err != nil || len(rows) != 1 || rows[0].ETASeconds != nil || rows[0].ETAUpperSeconds == nil {
		t.Fatal("retry time treated as normal duration", rows, err)
	}
}

func TestTaskDismissalHidesOnlyOwnFinishedTasksFromLists(t *testing.T) {
	db := finalizationDB(t)
	ctx := context.Background()
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid, ADD COLUMN cadence text`); err != nil {
		t.Fatal(err)
	}
	user, soul, other := uuid.New(), uuid.New(), uuid.New()
	task := func(owner uuid.UUID, status string) uuid.UUID {
		id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
		if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$3,strategy='direct',status=$4,completed_at=CASE WHEN $4<>'running' THEN now() END WHERE id=$1`, id, owner, soul, status); err != nil {
			t.Fatal(err)
		}
		return id
	}
	done, failed, active, foreign := task(user, "done"), task(user, "failed"), task(user, "running"), task(other, "done")
	control, reader := core.NewTaskController(db), core.NewTaskStatusReader(db)
	if _, err := control.Dismiss(ctx, user, soul, foreign); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("dismissed another owner's task", err)
	}
	if _, err := control.Dismiss(ctx, user, soul, active); !errors.Is(err, core.ErrTaskActive) {
		t.Fatal("dismissed active work", err)
	}
	if changed, err := control.Dismiss(ctx, user, soul, done); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if changed, err := control.Dismiss(ctx, user, soul, done); err != nil || changed {
		t.Fatal("repeated dismissal", changed, err)
	}
	list := func(owner uuid.UUID) map[uuid.UUID]bool {
		rows, err := reader.ReadPage(ctx, owner, soul, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[uuid.UUID]bool{}
		for _, row := range rows {
			seen[row.ID] = true
		}
		return seen
	}
	if seen := list(user); seen[done] || !seen[failed] || !seen[active] {
		t.Fatalf("list after single dismissal: %v", seen)
	}
	if rows, err := reader.Read(ctx, user, soul, done.String()); err != nil || len(rows) != 1 {
		t.Fatal("dismissed task lost its ID lookup", rows, err)
	}
	if n, err := control.DismissFinished(ctx, user, soul); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if seen := list(user); len(seen) != 1 || !seen[active] {
		t.Fatalf("list after clearing finished: %v", seen)
	}
	if seen := list(other); !seen[foreign] {
		t.Fatal("another owner's list changed")
	}
}

func TestTaskStatusExplicitSchemaDoesNotChangePoolSearchPath(t *testing.T) {
	db := finalizationDB(t)
	migration, err := migrations.ReadFile("sql/026_agent_task_steps.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid, ADD COLUMN cadence text`); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.Get(&schema, `SELECT current_schema()`); err != nil {
		t.Fatal(err)
	}
	user, soul := uuid.New(), uuid.New()
	id := insertPendingTask(t, context.Background(), db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$3,strategy='direct' WHERE id=$1`, id, user, soul); err != nil {
		t.Fatal(err)
	}
	shared, err := sqlx.Connect("postgres", os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	shared.SetMaxOpenConns(1)
	var before, after string
	if err := shared.Get(&before, `SHOW search_path`); err != nil {
		t.Fatal(err)
	}
	rows, err := core.NewTaskStatusReaderInSchema(shared, schema).Read(context.Background(), user, soul, id.String())
	if err != nil || len(rows) != 1 || rows[0].ID != id {
		t.Fatal(rows, err)
	}
	if err := shared.Get(&after, `SHOW search_path`); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("shared pool search_path changed: %s -> %s", before, after)
	}
}

func TestTaskStatusPagesIncludeRecentResultsAndAllActiveTasks(t *testing.T) {
	db := finalizationDB(t)
	migration, err := migrations.ReadFile("sql/026_agent_task_steps.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid, ADD COLUMN cadence text`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, soul := uuid.New(), uuid.New()
	for i := 0; i < 24; i++ {
		id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
		if _, err = db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$3,strategy='direct' WHERE id=$1`, id, user, soul); err != nil {
			t.Fatal(err)
		}
		if i >= 22 {
			if _, err = db.Exec(`UPDATE agent_tasks SET status='done',completed_at=now(),result='report' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	seen := map[uuid.UUID]bool{}
	reader := core.NewTaskStatusReader(db)
	for page := 0; page < 5; page++ {
		rows, err := reader.ReadPage(ctx, user, soul, page)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows[:min(5, len(rows))] {
			if seen[row.ID] {
				t.Fatal("duplicate task across pages")
			}
			seen[row.ID] = true
			if row.Result != nil {
				t.Fatal("list includes report body")
			}
		}
	}
	if len(seen) != 24 {
		t.Fatalf("lost tasks: %d", len(seen))
	}
	rows, err := reader.ReadPage(ctx, user, uuid.New(), 0)
	if err != nil || len(rows) != 0 {
		t.Fatal("scope leak", rows, err)
	}
	if _, err := reader.ReadPage(ctx, user, soul, -1); err == nil {
		t.Fatal("negative page accepted")
	}
}

func TestTaskStatusEstimatesFinalCheckFromMeasuredChecks(t *testing.T) {
	db := finalizationDB(t)
	ctx := context.Background()
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid, ADD COLUMN cadence text`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		history := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
		if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$2,strategy='direct',executor_version=2,status='done',completed_at=now() WHERE id=$1`, history, uuid.New()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,status,attempts,started_at,completed_at) VALUES($1,'report','Report','Complete','finalize','done',1,now()-interval '500 seconds',now()-interval '200 seconds')`, history); err != nil {
			t.Fatal(err)
		}
	}
	user, soul := uuid.New(), uuid.New()
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{"phase":"finalizing","final_validation":{"attempts":1}}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$3,strategy='direct',executor_version=2,status='running' WHERE id=$1`, id, user, soul); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,status,attempts,started_at,completed_at) VALUES($1,'report','Report','Complete','finalize','done',1,now()-interval '300 seconds',now()-interval '50 seconds')`, id); err != nil {
		t.Fatal(err)
	}
	rows, err := core.NewTaskStatusReader(db).Read(ctx, user, soul, id.String())
	if err != nil || len(rows) != 1 || rows[0].Stage != "verifying" || rows[0].ETASeconds == nil || *rows[0].ETASeconds < 145 || *rows[0].ETASeconds > 155 || rows[0].ETAUpperSeconds == nil {
		t.Fatalf("final check has no measured estimate: %+v %v", rows, err)
	}
}
