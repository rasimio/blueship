package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/rasimio/blueship/internal/core"
)

// toolOutputsDDL mirrors agent_task_tool_outputs from migration 011, minus
// its hardcoded `blueship.` schema prefix — that prefix is what stops the
// real migration set from running in a throwaway test schema.
const toolOutputsDDL = `
CREATE TABLE agent_task_tool_outputs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id       uuid NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
    iteration     integer NOT NULL,
    tool_name     text NOT NULL,
    tool_input    jsonb NOT NULL DEFAULT '{}',
    output        text NOT NULL,
    output_format text NOT NULL DEFAULT '',
    metadata      jsonb NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now()
)`

// A research task re-reads its own sources every iteration, because the
// bodies it fetched are never handed back to it — one production run spent
// 70 minutes on 196 fetches of which 102 were repeats, a single GitHub page
// pulled 13 times and a 36-page PDF 13 times, with headless-Chrome renders
// eating up to 98% of an iteration's wall clock.
//
// These pin what may be replayed and, more importantly, what may not.
func TestTaskFetchCacheReplaysWhatTheTaskAlreadyRead(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set BLUESHIP_TEST_POSTGRES_DSN to run PostgreSQL agent-task tests")
	}
	db := newAgentTaskSchema(t, dsn)
	if _, err := db.Exec(toolOutputsDDL); err != nil {
		t.Fatalf("create agent_task_tool_outputs: %v", err)
	}
	store := core.NewAgentTaskStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	task := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	other := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))

	const page = "https://github.com/unitreerobotics/unitree_ros2"
	insertFetchRow(t, ctx, db, task, 3, page, page, "README body", false, 0)

	t.Run("repeat request is answered from the store", func(t *testing.T) {
		got, err := store.LookupTaskFetch(ctx, task, page, 6*time.Hour)
		if err != nil {
			t.Fatalf("LookupTaskFetch: %v", err)
		}
		if got == nil || got.Output != "README body" {
			t.Fatalf("cache miss on a page this task read: %#v", got)
		}
		if got.Iteration != 3 {
			t.Errorf("cached iteration = %d, want the iteration that really fetched it (3)", got.Iteration)
		}
	})

	t.Run("the citation form of the URL finds the resolved row", func(t *testing.T) {
		// arxiv /abs/ is rewritten to /pdf/ before the fetch; the model asks
		// for the abstract page it cited.
		abs := "https://arxiv.org/abs/2503.14734"
		insertFetchRow(t, ctx, db, task, 4, abs, "https://arxiv.org/pdf/2503.14734", "paper text", false, 0)
		for _, ask := range []string{abs, "https://arxiv.org/pdf/2503.14734", "http://www.arxiv.org/abs/2503.14734/"} {
			got, err := store.LookupTaskFetch(ctx, task, ask, 6*time.Hour)
			if err != nil || got == nil || got.Output != "paper text" {
				t.Errorf("asking for %q missed the row it was fetched by (err=%v, got=%#v)", ask, err, got)
			}
		}
	})

	t.Run("another task's reading is not shared", func(t *testing.T) {
		got, err := store.LookupTaskFetch(ctx, other, page, 6*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Error("cache crossed task boundaries; a task must only replay its own reading")
		}
	})

	t.Run("a stale body is refetched", func(t *testing.T) {
		old := "https://example.test/old"
		insertFetchRow(t, ctx, db, task, 1, old, old, "yesterday", false, 7*time.Hour)
		got, err := store.LookupTaskFetch(ctx, task, old, 6*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Error("a body older than the TTL was replayed")
		}
	})

	// The invariant that keeps the TTL honest: rows the cache itself wrote
	// carry from_cache, and must never become the source of the next replay
	// — otherwise each hit renews the timestamp and a document lives forever.
	t.Run("a replay never renews a document's age", func(t *testing.T) {
		url := "https://example.test/laundered"
		insertFetchRow(t, ctx, db, task, 1, url, url, "real download", false, 7*time.Hour)
		insertFetchRow(t, ctx, db, task, 2, url, url, "replayed copy", true, time.Minute)
		got, err := store.LookupTaskFetch(ctx, task, url, 6*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Fatalf("a from_cache row kept an expired document alive: %q", got.Output)
		}
	})

	// Gate B' passes a resubmission only when every recheck URL was fetched
	// again inside the same iteration. Serving one from cache would make that
	// check prove nothing.
	t.Run("recheck URLs are excluded from the cache", func(t *testing.T) {
		if _, err := db.ExecContext(ctx,
			`UPDATE agent_tasks SET required_recheck_urls = $2 WHERE id = $1`,
			task, `{"https://github.com/unitreerobotics/unitree_ros2"}`); err != nil {
			t.Fatalf("set recheck urls: %v", err)
		}
		forced, err := store.TaskRequiresRefetch(ctx, task, page)
		if err != nil {
			t.Fatal(err)
		}
		if !forced {
			t.Error("a URL Gate C demanded be reread was not marked for refetch")
		}
		// Same document, written the way the model would type it.
		forced, err = store.TaskRequiresRefetch(ctx, task, page+"/")
		if err != nil {
			t.Fatal(err)
		}
		if !forced {
			t.Error("recheck matching is literal; a trailing slash escaped the gate")
		}
		if forced, err = store.TaskRequiresRefetch(ctx, task, "https://example.test/unrelated"); err != nil || forced {
			t.Errorf("unrelated URL marked for refetch (err=%v)", err)
		}
	})
}

func insertFetchRow(t *testing.T, ctx context.Context, db *sqlx.DB,
	taskID uuid.UUID, iteration int, requested, final, body string, fromCache bool, age time.Duration) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{
		"requested_url": requested,
		"final_url":     final,
		"title":         "doc",
		"page_count":    0,
		"from_cache":    fromCache,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO agent_task_tool_outputs
		    (task_id, iteration, tool_name, tool_input, output, output_format, metadata, created_at)
		VALUES ($1, $2, 'browser_fetch', $3::jsonb, $4, 'html', $5::jsonb, now() - make_interval(secs => $6))`,
		taskID, iteration, fmt.Sprintf(`{"url":%q}`, requested), body, string(meta), age.Seconds()); err != nil {
		t.Fatalf("insert fetch row: %v", err)
	}
}
