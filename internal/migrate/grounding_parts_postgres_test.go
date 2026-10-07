package migrate

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/core"
)

type persistedPartsReviewer struct {
	audits      atomic.Int32
	unavailable atomic.Bool
}

func (p *persistedPartsReviewer) Complete(_ context.Context, req core.CompletionRequest) (*core.CompletionResponse, error) {
	text := `{"met":true,"reason":"complete"}`
	if strings.HasPrefix(req.System, "You are a citation auditor") {
		p.audits.Add(1)
		user := core.ExtractText(core.NormalizeContent(req.Messages[len(req.Messages)-1].Content))
		_, target, _ := strings.Cut(user, "\n\n[audit_target]\n")
		if strings.HasPrefix(target, "B") && p.unavailable.Load() {
			return &core.CompletionResponse{StopReason: "max_tokens"}, nil
		}
		body, _ := json.Marshal(map[string]any{"claims": []map[string]string{{"claim": target[:1], "claim_type": "numerical", "status": "grounded", "supporting_doc_url": "https://example.com/source", "supporting_span": "evidence"}}})
		text = string(body)
	}
	return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: text}}}, nil
}

func TestGraphGroundingPartitionsResumeFromPostgres(t *testing.T) {
	db := finalizationDB(t)
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid DEFAULT gen_random_uuid(),ADD COLUMN cadence text,ADD COLUMN acceptance_criteria text`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"026_agent_task_steps.sql", "027_agent_task_executor_version.sql", "028_agent_task_runs.sql"} {
		data, err := migrations.ReadFile("sql/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(data)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	report := "A" + strings.Repeat("a", 2000) + "\nB" + strings.Repeat("b", 2000) + "\nC" + strings.Repeat("c", 2000)
	if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2,strategy='direct',deadline=now()+interval '10 minutes',acceptance_criteria='Verify all three sections' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,status,result) VALUES($1,'final','Report','Complete','finalize','done',$2)`, id, report); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE agent_task_tool_outputs(task_id uuid,tool_name text,tool_input jsonb,output text,output_format text,metadata jsonb,iteration int,created_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agent_task_tool_outputs VALUES($1,'browser_fetch','{"url":"https://example.com/source"}','evidence','html','{"final_url":"https://example.com/source","observed_at":"2026-09-22T19:00:00Z"}',1,now())`, id); err != nil {
		t.Fatal(err)
	}
	reviewer := &persistedPartsReviewer{}
	reviewer.unavailable.Store(true)
	cfg := &core.Config{}
	cfg.ApplyDefaults()
	cfg.Models.Primary.Name = "test-model"
	cfg.LLM = reviewer
	cfg.DB = os.Getenv("BLUESHIP_TEST_POSTGRES_DSN")
	if err := db.Get(&cfg.ShipSchema, `SELECT current_schema()`); err != nil {
		t.Fatal(err)
	}
	deps, err := core.InitDeps(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	deps.Prompts = core.NewMapPromptStore(map[string]string{"background-graph-acceptance": "Review graph result"})
	deps.EnsureAutonomousHistory = func(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }
	handler := &schedulerGraphStub{}
	run := func(deps *core.Deps) {
		scheduler := agenttask.NewScheduler(core.NewAgentTaskStore(db), nil, nil, core.NewToolRegistry(), nil, deps, nil, slog.New(slog.DiscardHandler))
		scheduler.SetGraphHandler(handler)
		if err := scheduler.Run(ctx); err != nil {
			t.Fatal(err)
		}
		scheduler.Wait()
	}
	run(deps)
	deps.Close()
	store := core.NewAgentTaskStore(db)
	task, err := store.Get(ctx, id)
	if err != nil || task.Status != "pending" || task.Result != nil || reviewer.audits.Load() != 3 {
		t.Fatal("incomplete audit published", task, err, reviewer.audits.Load())
	}
	var saved struct {
		Final struct {
			Verdict agenttask.AcceptanceVerdict `json:"verdict"`
		} `json:"final_validation"`
	}
	if err = json.Unmarshal(task.Progress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Final.Verdict.Grounding == nil || len(saved.Final.Verdict.Grounding.AuditedParts) != 2 || saved.Final.Verdict.GroundingInputHash == "" {
		t.Fatal("completed partitions absent from DB", string(task.Progress))
	}
	if _, err = db.Exec(`UPDATE agent_task_runs SET next_attempt_at=now()-interval '1 second' WHERE task_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	// Fresh dependency container, connection pools and scheduler: only persisted
	// task state can supply the two previously completed partition verdicts.
	reviewer.unavailable.Store(false)
	resumed, err := core.InitDeps(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumed.Prompts = deps.Prompts
	resumed.EnsureAutonomousHistory = deps.EnsureAutonomousHistory
	run(resumed)
	task, err = store.Get(ctx, id)
	if err != nil || task.Status != "done" || task.Result == nil || *task.Result != report || reviewer.audits.Load() != 4 || handler.planned != 0 || handler.executed != 0 {
		t.Fatal("restart repeated audits/research or lost report", task, err, reviewer.audits.Load(), handler)
	}
	var message string
	if err = db.Get(&message, `SELECT message_text FROM agent_task_notification_attempts WHERE task_id=$1`, id); err != nil || message != core.TaskReportNotification("Done: %s", task.Title, report, id) {
		t.Fatal("outbox artifact mismatch", err)
	}
}
