package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

func TestTaskRestartPreservesSourceAndSerializesDuplicateRequests(t *testing.T) {
	db := finalizationDB(t)
	for _, name := range []string{"026_agent_task_steps.sql", "029_agent_task_restarts.sql"} {
		data, err := migrations.ReadFile("sql/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid, ADD COLUMN cadence text, ADD COLUMN description text, ADD COLUMN acceptance_criteria text, ADD COLUMN delegate_to text, ADD COLUMN tools text[] NOT NULL DEFAULT '{}', ADD COLUMN use_agents text[] NOT NULL DEFAULT '{}', ADD COLUMN session_id text`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, soul := uuid.New(), uuid.New()
	control := core.NewTaskController(db)
	store := core.NewAgentTaskStore(db)
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$3,executor_version=2,strategy='direct',config='{"start_at":"2000-01-01T00:00:00Z","prompt":"original goal"}',deadline=now()-interval '1 day' WHERE id=$1`, id, user, soul); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Restart(ctx, user, soul, id, time.Hour); !errors.Is(err, core.ErrTaskNotTerminal) {
		t.Fatal("active task restarted", err)
	}
	if _, err := db.Exec(`UPDATE agent_tasks SET status='failed',result='saved partial',progress='{"phase":"terminal"}',session_id='old-session',iteration=9 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][2]uuid.UUID{{uuid.New(), soul}, {user, uuid.New()}, {uuid.Nil, soul}} {
		if _, err := control.Restart(ctx, scope[0], scope[1], id, time.Hour); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("foreign restart", err)
		}
	}
	if err := control.Resume(ctx, user, soul, id); !errors.Is(err, core.ErrTaskNotRecurring) {
		t.Fatal("revived one-shot", err)
	}
	if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,status,attempts) VALUES($1,'send','Send','Receipt','action','reconciliation',1)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Restart(ctx, user, soul, id, time.Hour); !errors.Is(err, core.ErrTaskRestartUncertain) {
		t.Fatal("uncertain effect repeated", err)
	}
	if _, err := db.Exec(`UPDATE agent_task_steps SET status='done',result='confirmed receipt' WHERE task_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	const clients = 5
	children := make([]uuid.UUID, clients)
	errs := make([]error, clients)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Add(1)
		go func(i int) { defer wg.Done(); children[i], errs[i] = control.Restart(ctx, user, soul, id, time.Hour) }(i)
	}
	wg.Wait()
	for i := range clients {
		if errs[i] != nil || children[i] == id || children[i] == uuid.Nil || children[i] != children[0] {
			t.Fatal(children, errs)
		}
	}
	source, err := store.Get(ctx, id)
	if err != nil || source.Status != "failed" || source.Result == nil || *source.Result != "saved partial" || source.Iteration != 9 || source.SessionID == nil {
		t.Fatal("source changed", source, err)
	}
	child, err := store.Get(ctx, children[0])
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != "pending" || child.ExecutorVersion != 2 || child.Iteration != 0 || child.Result != nil || child.SessionID != nil || child.LastRunAt != nil || child.Deadline != nil {
		t.Fatal("runtime state inherited", child)
	}
	var config map[string]any
	if err := json.Unmarshal(child.Config, &config); err != nil {
		t.Fatal(err)
	}
	if config["restart_of"] != id.String() || config["prompt"] != "original goal" || config["start_at"] != nil {
		t.Fatal(config)
	}
	var n int
	if err := db.Get(&n, `SELECT count(*) FROM agent_task_steps WHERE task_id=$1`, child.ID); err != nil || n != 0 {
		t.Fatal("graph inherited", n, err)
	}
	recurring := insertPendingTask(t, ctx, db, "heartbeat", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$3,status='canceled',schedule='*/30 * * * *' WHERE id=$1`, recurring, user, soul); err != nil {
		t.Fatal(err)
	}
	if err := control.Resume(ctx, uuid.New(), soul, recurring); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign schedule resumed", err)
	}
	if err := control.Resume(ctx, user, soul, recurring); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.Get(ctx, recurring)
	if err != nil || resumed.Status != "pending" {
		t.Fatal(resumed, err)
	}
	if _, err := control.Restart(ctx, user, soul, recurring, time.Hour); !errors.Is(err, core.ErrTaskNotTerminal) {
		t.Fatal("schedule restarted as one-shot", err)
	}

}
