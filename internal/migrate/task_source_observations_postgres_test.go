package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

func TestTaskSourceObservationsPreserveOriginalTimesAndTenantScope(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BLUESHIP_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set BLUESHIP_TEST_POSTGRES_DSN")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "15433" {
		t.Fatal("requires isolated PostgreSQL on 127.0.0.1:15433")
	}
	db := newAgentTaskSchema(t, dsn)
	migration, err := migrations.ReadFile("sql/011_tool_outputs.sql")
	if err != nil {
		t.Fatal(err)
	}
	// The production migration names blueship explicitly; this test uses the
	// helper's private search_path, including for the foreign key and cleanup.
	if _, err := db.Exec(strings.ReplaceAll(string(migration), "blueship.", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid;
	 ALTER TABLE agent_task_tool_outputs ADD COLUMN soul_id uuid`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, soul := uuid.New(), uuid.New()
	taskID := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	otherTaskID := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.ExecContext(ctx, `UPDATE agent_tasks SET user_id=$1,soul_id=$2 WHERE id IN ($3,$4)`, owner, soul, taskID, otherTaskID); err != nil {
		t.Fatal(err)
	}
	insertSource := func(task, sourceSoul uuid.UUID, toolName, inputURL, body string, metadata map[string]any, createdAt time.Time) {
		t.Helper()
		input, err := json.Marshal(map[string]string{"url": inputURL})
		if err != nil {
			t.Fatal(err)
		}
		meta, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO agent_task_tool_outputs
		 (task_id,soul_id,iteration,tool_name,tool_input,output,output_format,metadata,created_at)
		 VALUES($1,$2,1,$3,$4,$5,'html',$6,$7)`, task, sourceSoul, toolName, input, body, meta, createdAt); err != nil {
			t.Fatal(err)
		}
	}
	inputURL := "https://example.invalid/input"
	requestedURL := "https://example.invalid/requested"
	finalURL := "https://example.invalid/final"
	partialAt := time.Date(2026, 9, 22, 23, 55, 49, 123456789, time.FixedZone("source", 2*60*60))
	completeAt := partialAt.Add(2*time.Second + 17*time.Nanosecond)
	createdAt := completeAt.Add(time.Minute).UTC().Truncate(time.Microsecond)
	legacyAt := createdAt.Add(-time.Hour)
	insertSource(taskID, soul, "browser_fetch", inputURL, "Partial source body", map[string]any{
		"requested_url": requestedURL, "final_url": finalURL,
		"observed_at": partialAt.Format(time.RFC3339Nano), "partial": true,
	}, createdAt)
	insertSource(taskID, soul, "browser_fetch", inputURL, "Complete source body", map[string]any{
		"requested_url": requestedURL, "final_url": finalURL,
		"observed_at": completeAt.Format(time.RFC3339Nano), "from_cache": false,
	}, createdAt)
	insertSource(taskID, soul, "browser_fetch", "https://example.invalid/legacy", "Legacy source body", map[string]any{}, legacyAt)
	// A cached reread, an empty response and unrelated tools must not supply a
	// fresh observation. Nor may a row belonging to another task or soul.
	excludedAt := createdAt.Add(time.Hour)
	for _, row := range []struct {
		task, sourceSoul uuid.UUID
		toolName, body   string
		metadata         map[string]any
	}{
		{taskID, soul, "browser_fetch", "Cached source", map[string]any{"observed_at": excludedAt, "from_cache": true}},
		{taskID, soul, "browser_fetch", "", map[string]any{"observed_at": excludedAt}},
		{taskID, soul, "browser_search", "Search results", map[string]any{"observed_at": excludedAt}},
		{otherTaskID, soul, "browser_fetch", "Other task source", map[string]any{"observed_at": excludedAt}},
		{taskID, uuid.New(), "browser_fetch", "Other soul source", map[string]any{"observed_at": excludedAt}},
	} {
		insertSource(row.task, row.sourceSoul, row.toolName, inputURL, row.body, row.metadata, excludedAt)
	}
	store := core.NewAgentTaskStore(db)
	observations, err := store.TaskSourceObservations(ctx, taskID, owner, soul)
	if err != nil || len(observations) != 3 {
		t.Fatalf("observations=%+v err=%v; want three genuine observations", observations, err)
	}
	want := map[string][]string{
		partialAt.UTC().Format(time.RFC3339Nano):  {inputURL, requestedURL, finalURL},
		completeAt.UTC().Format(time.RFC3339Nano): {inputURL, requestedURL, finalURL},
		legacyAt.Format(time.RFC3339Nano):         {"https://example.invalid/legacy", "", ""},
	}
	for _, observation := range observations {
		stamp := observation.ObservedAt.Format(time.RFC3339Nano)
		urls, found := want[stamp]
		if !found || !reflect.DeepEqual(observation.URLs, urls) || observation.ObservedAt.Location() != time.UTC {
			t.Fatalf("changed source observation: %+v; expected=%v", observation, want)
		}
		delete(want, stamp)
	}
	if len(want) != 0 {
		t.Fatalf("lost distinct observation versions: %v", want)
	}
	for _, scope := range []struct {
		name              string
		task, owner, soul uuid.UUID
		wantNoRows        bool
	}{
		{"wrong owner", taskID, uuid.New(), soul, false},
		{"wrong soul", taskID, owner, uuid.New(), false},
		{"missing task", uuid.New(), owner, soul, false},
		{"nil task", uuid.Nil, owner, soul, true},
		{"nil owner", taskID, uuid.Nil, soul, true},
		{"nil soul", taskID, owner, uuid.Nil, true},
	} {
		t.Run(scope.name, func(t *testing.T) {
			observations, err := store.TaskSourceObservations(ctx, scope.task, scope.owner, scope.soul)
			if len(observations) != 0 || (scope.wantNoRows && !errors.Is(err, sql.ErrNoRows)) || (!scope.wantNoRows && err != nil) {
				t.Fatalf("scope leaked observations or returned unexpected error: %+v, %v", observations, err)
			}
		})
	}
}
