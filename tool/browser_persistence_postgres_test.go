package tool

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/webaccess/browser"
)

func TestCompletedSourceSurvivesTaskCancellation(t *testing.T) {
	dsn := os.Getenv("BLUESHIP_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL opt-in")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "15433" {
		t.Fatal("requires isolated local PostgreSQL15433")
	}
	admin, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "source_cancel_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec("CREATE SCHEMA " + pq.QuoteIdentifier(schema)); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec("DROP SCHEMA " + pq.QuoteIdentifier(schema) + " CASCADE")
	deps, err := bs.InitDeps(&bs.Config{DB: dsn, ShipSchema: schema}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer deps.Close()
	db, err := deps.DB("ship")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE agent_task_tool_outputs(soul_id uuid,task_id uuid,iteration int,tool_name text,tool_input jsonb,output text,output_format text,metadata jsonb,created_at timestamptz DEFAULT now()); CREATE TABLE agent_tasks(id uuid,required_recheck_urls text[])`); err != nil {
		t.Fatal(err)
	}
	soul, task := uuid.New(), uuid.New()
	ctx, cancel := context.WithCancel(bs.ContextWithIteration(bs.ContextWithTaskID(bs.WithSoulID(context.Background(), soul), task), 7))
	cancel()
	persistBrowserFetchOutput(ctx, deps, json.RawMessage(`{"url":"https://example.com/source"}`), &browser.FetchResult{URL: "https://example.com/source", Text: "Observed source before cancellation", SourceKind: "html", PartialError: "render interrupted after source read"})
	var gotSoul, gotTask uuid.UUID
	var iteration int
	var output string
	if err = db.QueryRow(`SELECT soul_id,task_id,iteration,output FROM agent_task_tool_outputs`).Scan(&gotSoul, &gotTask, &iteration, &output); err != nil {
		t.Fatal("completed evidence lost on cancellation", err)
	}
	if gotSoul != soul || gotTask != task || iteration != 7 || output != "Observed source before cancellation" {
		t.Fatal("source attribution changed", gotSoul, gotTask, iteration, output)
	}
	cacheCtx := bs.ContextWithTaskID(bs.WithSoulID(context.Background(), soul), task)
	cached := replayTaskFetch(cacheCtx, deps, "https://example.com/source")
	if cached != nil {
		t.Fatal("partial extraction must not prevent a recovery fetch", cached)
	}
	observedAt := time.Now().UTC().Add(-time.Minute)
	persistBrowserFetchOutput(cacheCtx, deps, json.RawMessage(`{"url":"https://example.com/source"}`), &browser.FetchResult{URL: "https://example.com/source", Text: "Recovered complete source", SourceKind: "html", ObservedAt: &observedAt})
	cached = replayTaskFetch(cacheCtx, deps, "https://example.com/source")
	if cached == nil || !cached.FromCache || cached.Text != "Recovered complete source" {
		t.Fatal("successful recovery was not cached", cached)
	}
	if cached.ObservedAt == nil || !cached.ObservedAt.Equal(observedAt) {
		t.Fatal("cache renewed source observation time", cached.ObservedAt)
	}
	window, err := json.Marshal(excerptBrowserFetch(cached, 3, 8, ""))
	if err != nil || !strings.Contains(string(window), observedAt.Format(time.RFC3339Nano)) {
		t.Fatal("model-visible window lost source observation time", string(window), err)
	}
	persistBrowserFetchOutput(cacheCtx, deps, json.RawMessage(`{"url":"https://example.com/source"}`), cached)
	cached = replayTaskFetch(cacheCtx, deps, "https://example.com/source")
	if cached == nil || cached.ObservedAt == nil || !cached.ObservedAt.Equal(observedAt) {
		t.Fatal("persisted cache window renewed source time", cached)
	}
	// Source rows saved before observed_at existed retain their original DB
	// timestamp, rather than acquiring the current time on replay.
	var originalStoredAt time.Time
	if err := db.QueryRow(`UPDATE agent_task_tool_outputs SET metadata=metadata-'observed_at' WHERE output='Recovered complete source' AND NOT (metadata->>'from_cache')::boolean RETURNING created_at`).Scan(&originalStoredAt); err != nil {
		t.Fatal(err)
	}
	cached = replayTaskFetch(cacheCtx, deps, "https://example.com/source")
	if cached == nil || cached.ObservedAt == nil || !cached.ObservedAt.Equal(originalStoredAt) {
		t.Fatal("legacy source lost original timestamp", cached)
	}
	persistToolOutput(context.Background(), deps, bs.ToolOutputRecord{ToolName: ToolBrowserFetch, Output: "Chat must not enter task audit"})
	var count int
	if err = db.Get(&count, `SELECT count(*) FROM agent_task_tool_outputs`); err != nil || count != 3 {
		t.Fatal("chat output attributed to a task", count, err)
	}
	registry := bs.NewToolRegistry()
	if err := RegisterBrowserTools(registry, deps); err != nil {
		t.Fatal(err)
	}
	evidence := registry.EvidenceOnlySubset()
	if defs := evidence.Definitions(); len(defs) != 1 || defs[0].Name != ToolBrowserFetch {
		t.Fatal("external tools in evidence registry", defs)
	}
	text, failed := evidence.Execute(cacheCtx, ToolBrowserFetch, json.RawMessage(`{"url":"https://example.com/source","offset_chars":10,"limit_chars":8}`))
	if failed || !strings.Contains(text, "complete") || !strings.Contains(text, originalStoredAt.UTC().Format(time.RFC3339Nano)) {
		t.Fatal("saved evidence unavailable or clock changed", text, failed)
	}
	text, failed = evidence.Execute(cacheCtx, ToolBrowserFetch, json.RawMessage(`{"url":"https://example.com/source","queries":["Recovered","complete","missing"],"limit_chars":30}`))
	var multi browserExcerpt
	if err := json.Unmarshal([]byte(text), &multi); err != nil || failed || len(multi.QueryExcerpts) != 3 || !multi.QueryExcerpts[0].Found || !multi.QueryExcerpts[1].Found || multi.QueryExcerpts[2].Found || multi.ObservedAt == nil || !multi.ObservedAt.Equal(originalStoredAt) {
		t.Fatal("cached multi-query lost evidence or original clock", text, failed, err)
	}
	var windows int
	if err := db.Get(&windows, `SELECT jsonb_array_length(metadata->'read_offsets_chars') FROM agent_task_tool_outputs WHERE tool_input ? 'queries' ORDER BY created_at DESC LIMIT 1`); err != nil || windows != 2 {
		t.Fatal("query windows not persisted for audit", windows, err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	input, _ := json.Marshal(map[string]string{"url": server.URL})
	text, failed = evidence.Execute(cacheCtx, ToolBrowserFetch, input)
	if !failed || !strings.Contains(text, "saved source unavailable") || requests.Load() != 0 {
		t.Fatal("cache miss performed external research", text, failed, requests.Load())
	}
}
