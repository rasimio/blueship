package migrate

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/agenttask"
)

func TestQueueNotificationsCommitFilterReconnectAndStop(t *testing.T) {
	db := finalizationDB(t)
	data, err := migrations.ReadFile("sql/030_agent_task_queue_notify.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.Get(&schema, `SELECT current_schema()`); err != nil {
		t.Fatal(err)
	}
	dsn, err := url.Parse(os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	app := "task_queue_test_" + uuid.NewString()
	query := dsn.Query()
	query.Set("application_name", app)
	query.Set("connect_timeout", "2")
	dsn.RawQuery = query.Encode()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wakes := make(chan struct{}, 20)
	done := make(chan struct{})
	go func() {
		defer close(done)
		agenttask.ListenQueue(ctx, dsn.String(), schema, func() { wakes <- struct{}{} }, slog.New(slog.DiscardHandler))
	}()
	expect := func() {
		t.Helper()
		select {
		case <-wakes:
		case <-time.After(3 * time.Second):
			t.Fatal("no queue wake before polling interval")
		}
	}
	quiet := func() {
		t.Helper()
		select {
		case <-wakes:
			t.Fatal("unexpected queue notification")
		case <-time.After(80 * time.Millisecond):
		}
	}
	expect() // LISTEN established; initial rescan closes startup race.
	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO agent_tasks(user_id,title,handler) VALUES($1,'uncommitted','background')`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	quiet()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	quiet()
	var id uuid.UUID
	if err := db.Get(&id, `INSERT INTO agent_tasks(user_id,title,handler) VALUES($1,'committed','background') RETURNING id`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	expect()
	if _, err := db.Exec(`UPDATE agent_tasks SET status='running' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE agent_tasks SET status='pending' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	quiet() // A retry transition must not make a hot loop.
	if _, err := db.Exec(`SELECT pg_notify('blueship_agent_task_queue','different_schema')`); err != nil {
		t.Fatal(err)
	}
	quiet()
	if _, err := db.Exec(`UPDATE agent_tasks SET status='done' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	expect()
	if _, err := db.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name=$1`, app); err != nil {
		t.Fatal(err)
	}
	expect() // lib/pq emits nil after reconnect; rescan recovers missed commits.
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("queue listener did not stop")
	}
}
