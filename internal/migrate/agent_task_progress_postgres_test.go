package migrate

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"github.com/rasimio/blueship/internal/core"
)

// preMigration022AgentTasks is agent_tasks as it stood before migration 022:
// progress and config nullable with a DEFAULT that only fires when the column
// is omitted. Columns are limited to what the statements under test touch —
// `SELECT *` into core.AgentTask tolerates a struct field with no column, but
// not a column with no field.
//
// Hand-written rather than replayed from the embedded migration set: three of
// those files (009/010/011) hardcode a `blueship.` schema prefix, so Run only
// succeeds in a schema by that exact name, and this test wants an isolated
// throwaway schema it can drop. TestInitSchemaDeclaresAgentTaskStateNotNull
// covers the fresh-install shape that this fixture cannot.
const preMigration022AgentTasks = `
CREATE TABLE agent_tasks (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id               UUID NOT NULL,
    title                 TEXT NOT NULL,
    handler               TEXT NOT NULL,
    strategy              TEXT NOT NULL DEFAULT 'recurring',
    config                JSONB DEFAULT '{}',
    plan                  JSONB NOT NULL DEFAULT '{}',
    progress              JSONB DEFAULT '{}',
    status                TEXT NOT NULL DEFAULT 'pending',
    result                TEXT,
    error_message         TEXT,
    iteration             INT NOT NULL DEFAULT 0,
    max_iterations        INT NOT NULL DEFAULT 10,
    required_recheck_urls TEXT[] NOT NULL DEFAULT '{}',
    schedule              TEXT,
    deadline              TIMESTAMPTZ,
    last_run_at           TIMESTAMPTZ,
    completed_at          TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`

// TestAgentTaskProgressSurvivesEmptyCheckpoint pins the invariant that took
// the production scheduler down: a handler iteration with nothing to
// checkpoint hands UpdateProgress a nil json.RawMessage, and the nil reached
// the driver as SQL NULL. json.RawMessage implements no sql.Scanner, so the
// next PendingTasks — `SELECT *` — died on that row with "unsupported Scan,
// storing driver.Value type <nil>", and kept dying every tick, for every
// task, until the row was repaired by hand.
//
// Runs against real PostgreSQL because the bug lived in the gap between what
// the writer sent and what the column allowed, which is precisely what a
// mocked database agrees to hide.
func TestAgentTaskProgressSurvivesEmptyCheckpoint(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set BLUESHIP_TEST_POSTGRES_DSN to run PostgreSQL agent-task tests")
	}
	db := newAgentTaskSchema(t, dsn)
	store := core.NewAgentTaskStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The production casualty, reproduced: a pending task whose progress is
	// already NULL when the migration arrives.
	casualty := insertPendingTask(t, ctx, db, "self-perception", nil)

	migration, err := migrations.ReadFile("sql/022_agent_task_state_not_null.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(migration)); err != nil {
		t.Fatalf("apply migration 022: %v", err)
	}

	var repaired json.RawMessage
	if err := db.QueryRowContext(ctx,
		`SELECT progress FROM agent_tasks WHERE id = $1`, casualty).Scan(&repaired); err != nil {
		t.Fatalf("read repaired progress: %v", err)
	}
	if string(repaired) != "{}" {
		t.Fatalf("progress after migration = %q, want {} — the stuck row was not repaired", repaired)
	}

	// The write path: an iteration that checkpoints nothing. The row has to be
	// RUNNING first — UpdateProgress only advances a task that is still running,
	// so against a pending row it would update nothing and this would pass
	// while testing none of the statement it exists to cover.
	task := insertPendingTask(t, ctx, db, "self-perception", json.RawMessage(`{"phase":"iteration_1"}`))
	setTaskStatus(t, ctx, db, task, "running")
	if err := store.UpdateProgress(ctx, task, nil); err != nil {
		t.Fatalf("UpdateProgress with no checkpoint: %v", err)
	}
	// The read path: the scheduler's own query, which is what actually fell over.
	tasks, err := store.PendingTasks(ctx)
	if err != nil {
		t.Fatalf("PendingTasks after empty checkpoint: %v", err)
	}
	var found *core.AgentTask
	for i := range tasks {
		if tasks[i].ID == task {
			found = &tasks[i]
		}
	}
	if found == nil {
		t.Fatalf("task %s missing from %d pending tasks", task, len(tasks))
	}
	if !json.Valid(found.Progress) {
		t.Fatalf("progress = %q, want valid json", found.Progress)
	}

	// PauseTask writes the same column from the same nil, and the recheck
	// variant is a separate statement once the URL list is non-empty — so
	// neither is covered by the check above.
	if err := store.PauseTask(ctx, task, nil); err != nil {
		t.Fatalf("PauseTask with no checkpoint: %v", err)
	}
	setTaskStatus(t, ctx, db, task, "running") // PauseTask above left it paused
	if err := store.UpdateProgressWithRecheck(ctx, task, nil, []string{"https://example.test/doc"}); err != nil {
		t.Fatalf("UpdateProgressWithRecheck with no checkpoint: %v", err)
	}
	if _, err := store.PendingTasks(ctx); err != nil {
		t.Fatalf("PendingTasks after recheck checkpoint: %v", err)
	}

	// And the constraint holds against a writer that bypasses the store —
	// the half that makes the invariant enforced rather than agreed upon.
	for _, column := range []string{"progress", "config"} {
		if _, err := db.ExecContext(ctx,
			`UPDATE agent_tasks SET `+column+` = NULL WHERE id = $1`, task); err == nil {
			t.Fatalf("%s accepted NULL — migration 022 left the column nullable", column)
		}
	}
}

func newAgentTaskSchema(t *testing.T, dsn string) *sqlx.DB {
	t.Helper()
	admin, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	schema := "agent_task_progress_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(`CREATE SCHEMA ` + pq.QuoteIdentifier(schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}

	testDSN, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	query := testDSN.Query()
	query.Set("search_path", schema)
	testDSN.RawQuery = query.Encode()
	db, err := sqlx.Connect("postgres", testDSN.String())
	if err != nil {
		t.Fatalf("connect test schema: %v", err)
	}

	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + pq.QuoteIdentifier(schema) + ` CASCADE`)
		admin.Close()
	})

	if _, err := db.Exec(preMigration022AgentTasks); err != nil {
		t.Fatalf("create pre-022 agent_tasks: %v", err)
	}
	return db
}

// insertPendingTask writes straight through SQL rather than through Create:
// the store's insert normalises the very field under test.
func insertPendingTask(t *testing.T, ctx context.Context, db *sqlx.DB, handler string, progress json.RawMessage) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRowContext(ctx, `
		INSERT INTO agent_tasks (user_id, title, handler, status, progress)
		VALUES ($1, $2, $3, 'pending', $4)
		RETURNING id`,
		uuid.New(), "progress regression", handler, progress).Scan(&id); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return id
}

func setTaskStatus(t *testing.T, ctx context.Context, db *sqlx.DB, id uuid.UUID, status string) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`UPDATE agent_tasks SET status = $2 WHERE id = $1`, id, status); err != nil {
		t.Fatalf("set task status %s: %v", status, err)
	}
}

// A task cancelled while an iteration is in flight must stay cancelled.
//
// The live sequence: the person said "отмени задачу", Cancel wrote 'done', and
// forty seconds later the iteration that was already running finished, saved
// its progress, and put the row back to 'pending' — where the scheduler picked
// it up and started the next iteration. The cancellation was real and lasted
// less than a minute.
func TestCancelDuringAnIterationIsNotUndoneByItsProgressWrite(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set BLUESHIP_TEST_POSTGRES_DSN to run PostgreSQL agent-task tests")
	}
	db := newAgentTaskSchema(t, dsn)
	store := core.NewAgentTaskStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task := insertPendingTask(t, ctx, db, "direct", json.RawMessage(`{"phase":"iteration_1"}`))
	setTaskStatus(t, ctx, db, task, "running")

	if err := store.Cancel(ctx, task); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// The iteration that was already running now finishes and checkpoints.
	if err := store.UpdateProgress(ctx, task, json.RawMessage(`{"phase":"iteration_2"}`)); err != nil {
		t.Fatalf("progress write after cancel returned an error; it should be a silent no-op: %v", err)
	}

	after, err := store.Get(ctx, task)
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if after.Status == "pending" {
		t.Fatalf("cancelled task was returned to the queue by its own iteration finishing (status=%s)", after.Status)
	}
	if after.Status != "done" {
		t.Fatalf("status = %q, want the cancellation to stand as done", after.Status)
	}

	// And the same for the retry path: an iteration that FAILS after a cancel
	// must not requeue it either.
	// Not nil: this fixture is the pre-022 schema where progress is still
	// nullable, and a NULL cannot be scanned back into json.RawMessage.
	task2 := insertPendingTask(t, ctx, db, "direct", json.RawMessage(`{}`))
	setTaskStatus(t, ctx, db, task2, "running")
	if err := store.Cancel(ctx, task2); err != nil {
		t.Fatalf("cancel second task: %v", err)
	}
	if err := store.SetPending(ctx, task2); err != nil {
		t.Fatalf("SetPending after cancel: %v", err)
	}
	after2, err := store.Get(ctx, task2)
	if err != nil {
		t.Fatalf("refetch second: %v", err)
	}
	if after2.Status == "pending" {
		t.Fatal("a cancelled task was requeued by the iteration-failed retry path")
	}
}

// The guard must not break the ordinary case: a running iteration that
// finishes normally still advances.
func TestRunningTaskStillAdvancesOnProgress(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set BLUESHIP_TEST_POSTGRES_DSN to run PostgreSQL agent-task tests")
	}
	db := newAgentTaskSchema(t, dsn)
	store := core.NewAgentTaskStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task := insertPendingTask(t, ctx, db, "direct", json.RawMessage(`{"phase":"iteration_1"}`))
	setTaskStatus(t, ctx, db, task, "running")
	if err := store.UpdateProgress(ctx, task, json.RawMessage(`{"phase":"iteration_2"}`)); err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}
	after, err := store.Get(ctx, task)
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if after.Status != "pending" {
		t.Fatalf("status = %q, want pending so the scheduler runs the next iteration", after.Status)
	}
	if after.Iteration != 1 {
		t.Fatalf("iteration = %d, want 1", after.Iteration)
	}
}
