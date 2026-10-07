package migrate

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/runtime/session"
)

func TestTerminalGraphSessionsArchiveWithoutLosingEvidence(t *testing.T) {
	db := finalizationDB(t)
	_, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid,ADD COLUMN cadence text;
 CREATE TABLE chat_sessions(id uuid PRIMARY KEY DEFAULT gen_random_uuid(),user_id uuid,soul_id uuid,source_id uuid,source text,active boolean DEFAULT true,updated_at timestamptz DEFAULT now());
 CREATE TABLE chat_messages(session_id uuid,content text)`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, soul := uuid.New(), uuid.New()
	terminal := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	running := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err = db.Exec(`UPDATE agent_tasks SET user_id=$1,soul_id=$2,strategy='direct',executor_version=2`, user, soul); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE agent_tasks SET status='failed',error_message='cancelled' WHERE id=$1`, terminal); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		var id uuid.UUID
		if err = db.Get(&id, `INSERT INTO chat_sessions(user_id,soul_id,source_id,source) VALUES ($1,$2,$3,'agent_task') RETURNING id`, user, soul, terminal); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO chat_messages VALUES ($1,'preserved evidence')`, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct {
		user, soul, task uuid.UUID
		source           string
	}{{user, soul, running, "agent_task"}, {uuid.New(), soul, terminal, "agent_task"}, {user, uuid.New(), terminal, "agent_task"}, {user, soul, terminal, "chat"}} {
		if _, err = db.Exec(`INSERT INTO chat_sessions(user_id,soul_id,source_id,source) VALUES ($1,$2,$3,$4)`, entry.user, entry.soul, entry.task, entry.source); err != nil {
			t.Fatal(err)
		}
	}
	store := session.NewStore(db)
	for _, want := range []int64{2, 1, 0} {
		n, err := store.ArchiveTerminalTaskSessions(ctx, 2)
		if err != nil || n != want {
			t.Fatal(n, err)
		}
	}
	var active, messages int
	if err = db.Get(&active, `SELECT count(*) FROM chat_sessions WHERE active`); err != nil {
		t.Fatal(err)
	}
	if err = db.Get(&messages, `SELECT count(*) FROM chat_messages`); err != nil {
		t.Fatal(err)
	}
	if active != 4 || messages != 3 {
		t.Fatal("scope or evidence lost", active, messages)
	}
}
