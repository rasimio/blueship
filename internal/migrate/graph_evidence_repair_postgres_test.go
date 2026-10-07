package migrate

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/handler"
	"github.com/rasimio/blueship/runtime/session"
	"github.com/rasimio/blueship/tool"
)

type evidenceRepairReviewer struct {
	t        *testing.T
	calls    int
	final    bool
	observed string
}

func (p *evidenceRepairReviewer) Complete(_ context.Context, req core.CompletionRequest) (*core.CompletionResponse, error) {
	if req.System == "review saved evidence" {
		input := core.ExtractText(core.NormalizeContent(req.Messages[0].Content))
		if !strings.Contains(input, "165 mm") || !strings.Contains(input, "observed_at") {
			p.t.Fatal("review lost saved-source receipt")
		}
		if strings.Contains(input, "{{observed_at:") || !strings.Contains(input, p.observed) {
			p.t.Fatal("review received unresolved or changed source observation")
		}
		return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"accepted":true,"reason":"saved source confirms 165 mm"}`}}}, nil
	}
	p.calls++
	if p.calls == 1 {
		if len(req.Tools) != 1 || req.Tools[0].Name != "browser_fetch" {
			p.t.Fatal("late step exposed external tools", req.Tools)
		}
		return &core.CompletionResponse{StopReason: "tool_use", Content: []core.ContentBlock{{Type: "tool_use", ID: "saved-height", Name: "browser_fetch", Input: json.RawMessage(`{"url":"https://example.invalid/case","query":"Cooler height","limit_chars":120}`)}}}, nil
	}
	if p.final {
		raw, err := json.Marshal(req.Messages)
		if err != nil {
			p.t.Fatal(err)
		}
		found := strings.Contains(string(raw), "165 mm")
		if !found {
			p.t.Fatal("final repair did not receive saved source")
		}
		return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: `{"edits":[{"before":"Height missing","after":"Height limit 165 mm; observed {{observed_at: https://example.invalid/case}}"}]}`}}}, nil
	}
	return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "Cooler height limit is 165 mm, confirmed in saved source; observed {{observed_at: https://example.invalid/case}}."}}}, nil
}

func TestGraphLateRepairReadsPostgresEvidenceWithoutNetwork(t *testing.T) {
	dsn := os.Getenv("BLUESHIP_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "15433" {
		t.Fatal("requires isolated15433")
	}
	admin, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	database := "evidence_repair_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec("CREATE DATABASE " + pq.QuoteIdentifier(database)); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec("DROP DATABASE " + pq.QuoteIdentifier(database) + " WITH (FORCE)")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	u.Path = "/" + database
	q := u.Query()
	q.Set("search_path", "blueship,public")
	u.RawQuery = q.Encode()
	reviewer := &evidenceRepairReviewer{t: t}
	cfg := &core.Config{DB: u.String(), ShipSchema: "blueship", LLM: reviewer}
	cfg.ApplyDefaults()
	cfg.Models.Primary.Name = "test-model"
	logger := slog.New(slog.DiscardHandler)
	deps, err := core.InitDeps(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deps.Close)
	db, err := deps.DB("ship")
	if err != nil {
		t.Fatal(err)
	}
	if err = Run(db, logger); err != nil {
		t.Fatal(err)
	}
	// Host-owned tenancy/session columns mirror the isolated live harness.
	if _, err = db.Exec(`ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS soul_id uuid;
	 ALTER TABLE agent_task_tool_outputs ADD COLUMN IF NOT EXISTS soul_id uuid;
	 ALTER TABLE chat_sessions ADD COLUMN IF NOT EXISTS soul_id uuid,ADD COLUMN IF NOT EXISTS last_input_tokens integer;
	 ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS soul_id uuid,ADD COLUMN IF NOT EXISTS visible_text text,
	 ADD COLUMN IF NOT EXISTS projection_status text NOT NULL DEFAULT '',ADD COLUMN IF NOT EXISTS projection_reason text,
	 ADD COLUMN IF NOT EXISTS projector_version text NOT NULL DEFAULT '',
	 ADD COLUMN IF NOT EXISTS reply_to_message_id uuid,ADD COLUMN IF NOT EXISTS tg_message_id bigint,ADD COLUMN IF NOT EXISTS tg_message_ids bigint[]`); err != nil {
		t.Fatal(err)
	}
	user, soul := uuid.New(), uuid.New()
	if _, err = db.Exec(`INSERT INTO user_profiles(id,chat_id,display_name) VALUES($1,'test:evidence-repair','Test')`, user); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	task, err := core.NewAgentTaskStore(db).Create(core.WithSoulID(context.Background(), soul), core.AgentTask{UserID: user, Title: "Verify cooler", Handler: "background", Strategy: core.StrategyDirect, ExecutorVersion: 2, Deadline: &deadline})
	if err != nil {
		t.Fatal(err)
	}
	task.CreatedAt = time.Now().Add(-8 * time.Minute)
	// One-shot graph tasks run to completion; the evidence-only repair window
	// exists only for time-bounded work, so exercise it on such a task.
	task.Strategy = core.StrategyRecurring
	observed := time.Now().UTC().Add(-5 * time.Minute)
	reviewer.observed = observed.Format(time.RFC3339Nano)
	source := strings.Repeat("navigation ", 700) + "Cooler height: 165 mm. SAVED_SOURCE_END"
	metadata, _ := json.Marshal(map[string]any{"final_url": "https://example.invalid/case", "requested_url": "https://example.invalid/case", "observed_at": observed, "read_offset_chars": 0})
	if _, err = db.Exec(`INSERT INTO agent_task_tool_outputs(task_id,soul_id,iteration,tool_name,tool_input,output,output_format,metadata) VALUES($1,$2,1,'browser_fetch','{"url":"https://example.invalid/case"}',$3,'html',$4)`, task.ID, soul, source, metadata); err != nil {
		t.Fatal(err)
	}
	registry := core.NewToolRegistry()
	if err = tool.RegisterBrowserTools(registry, deps); err != nil {
		t.Fatal(err)
	}
	prompts := core.NewMapPromptStore(map[string]string{"background-graph-step": "repair saved evidence", "background-graph-review": "review saved evidence"})
	ad := core.AgentDeps{Config: cfg, LLM: reviewer, Registry: registry, Store: session.NewStore(db), Prompts: prompts, Logger: logger, DB: deps.DB}
	checkpoint := json.RawMessage(`{"phase":"rejected","candidate_ready":true,"output":"Height missing","review_reason":"Read the saved case source"}`)
	result, accepted, err := handler.NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(core.WithSoulID(context.Background(), soul), task, core.TaskStep{ID: "verify", Kind: "read", Tools: []string{"browser_fetch", "browser_search"}, Checkpoint: checkpoint, Attempts: 2}, nil, ad, func(context.Context, json.RawMessage) error { return nil })
	if err != nil || !accepted || reviewer.calls != 2 || !strings.Contains(result.Output, "165 mm") || !strings.Contains(result.Output, reviewer.observed) || strings.Contains(result.Output, "{{observed_at:") {
		t.Fatal("saved-source repair failed", result, accepted, err, reviewer.calls)
	}
	raw, err := captureLiveSources(context.Background(), db, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sources []struct {
		Output   string         `json:"output"`
		Metadata map[string]any `json:"metadata"`
	}
	if err = json.Unmarshal(raw, &sources); err != nil || len(sources) != 2 {
		t.Fatal("source capture lost reads", err, len(sources))
	}
	for _, s := range sources {
		if s.Output != source || s.Metadata["observed_at"] != observed.Format(time.RFC3339Nano) {
			t.Fatal("capture changed full source or observation", s.Metadata)
		}
	}
	if sources[1].Metadata["read_offset_chars"].(float64) <= 6000 {
		t.Fatal("deep cached window not recorded", sources[1].Metadata)
	}
	reviewer.final, reviewer.calls = true, 0
	ad.Prompts = core.NewMapPromptStore(map[string]string{"background-graph-repair": "repair final report from saved evidence"})
	repaired, err := handler.NewBackground(time.UTC, nil, nil, nil).RepairGraphResult(context.Background(), task, nil, ad, "Height missing", "Read the saved case source")
	if err != nil || repaired != "Height limit 165 mm; observed "+reviewer.observed || reviewer.calls != 2 {
		t.Fatal("final repair could not read persisted task evidence", repaired, err, reviewer.calls)
	}
}
