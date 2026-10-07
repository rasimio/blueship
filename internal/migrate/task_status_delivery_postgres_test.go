package migrate

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

func TestTaskStatusDistinguishesArtifactFromDelivery(t *testing.T) {
	db := finalizationDB(t)
	ctx := context.Background()
	migration, err := migrations.ReadFile("sql/026_agent_task_steps.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid,ADD COLUMN cadence text`); err != nil {
		t.Fatal(err)
	}
	user, soul, attempt := uuid.New(), uuid.New(), uuid.New()
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET user_id=$2,soul_id=$3,status='done',strategy='direct',result='Verified report' WHERE id=$1`, id, user, soul); err != nil {
		t.Fatal(err)
	}
	reader := core.NewTaskStatusReader(db)
	check := func(want string, next, confirmed bool) {
		t.Helper()
		rows, err := reader.Read(ctx, user, soul, id.String())
		if err != nil || len(rows) != 1 {
			t.Fatal(rows, err)
		}
		s := rows[0]
		if s.Outcome != "completed" || s.Result == nil || *s.Result != "Verified report" || s.DeliveryState != want || (s.DeliveryNextAttemptAt != nil) != next || (s.DeliveryConfirmedAt != nil) != confirmed {
			t.Fatalf("status conflates execution/delivery: %+v", s)
		}
	}
	check("unknown", false, false)
	if _, err := db.Exec(`INSERT INTO agent_task_artifacts(task_id,version,outcome,body) VALUES($1,1,'completed','Verified report')`, id); err != nil {
		t.Fatal(err)
	}
	check("not_requested", false, false)
	if _, err := db.Exec(`INSERT INTO agent_task_notification_attempts(id,task_id,user_id,occurrence_key,message_text) VALUES($1,$2,$3,'result','Verified report')`, attempt, id, user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agent_task_notification_attempt_items(attempt_id,task_id,input_id,item_key) VALUES($1,$2,'task_result','version:1')`, attempt, id); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		state, want string
		attempts    int
	}{
		{"retryable", "pending", 0}, {"retryable", "retrying", 2}, {"dispatching", "sending", 1}, {"sent", "sent", 1}, {"uncertain", "uncertain", 1}, {"rejected", "undeliverable", 1},
	} {
		if _, err := db.Exec(`UPDATE agent_task_notification_attempts SET state=$2,attempt_count=$3,
 receipt=CASE WHEN $2='sent' THEN '{"transport":"telegram","message_id":"42"}'::jsonb END,
 error_message=CASE WHEN $2 IN ('retryable','uncertain','rejected') THEN 'provider failure' END,
 next_attempt_at=CASE WHEN $2='retryable' THEN now()+interval '1 minute' END,
 resolved_at=CASE WHEN $2 IN ('sent','uncertain','rejected') THEN now() END WHERE id=$1`, attempt, tc.state, tc.attempts); err != nil {
			t.Fatal(err)
		}
		check(tc.want, tc.state == "retryable", tc.state == "sent")
		if tc.state == "dispatching" {
			if _, err := db.Exec(`UPDATE agent_task_notification_attempts SET last_attempt_at=now()-interval '2 minutes' WHERE id=$1`, attempt); err != nil {
				t.Fatal(err)
			}
			check("uncertain", false, false)
		}
	}
	// A later artifact must not inherit the earlier version's receipt.
	if _, err := db.Exec(`INSERT INTO agent_task_artifacts(task_id,version,outcome,body) VALUES($1,2,'completed','Verified report')`, id); err != nil {
		t.Fatal(err)
	}
	check("not_requested", false, false)
	if rows, err := reader.Read(ctx, uuid.New(), soul, id.String()); err == nil || len(rows) > 0 {
		t.Fatal("foreign delivery leak", rows, err)
	}
}
