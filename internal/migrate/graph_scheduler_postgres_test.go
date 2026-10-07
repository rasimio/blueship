package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/core"
)

type repairGraphStub struct {
	schedulerGraphStub
	repairs int
	fail    bool
}

type repairAcceptanceStub struct {
	calls        int
	audits       atomic.Int32
	auditStarted chan struct{}
	partialAudit bool
}

func (r *repairAcceptanceStub) Complete(ctx context.Context, req core.CompletionRequest) (*core.CompletionResponse, error) {
	if (r.auditStarted != nil || r.partialAudit) && req.System != "Review graph result" {
		if r.audits.Add(1) == 1 && r.auditStarted != nil {
			close(r.auditStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"claims":[{"claim":"Revised report","claim_type":"framing","status":"grounded","supporting_span":"Revised report","supporting_doc_url":"https://example.com/source"}]}`}}}, nil
	}
	r.calls++
	if r.auditStarted != nil && r.calls == 1 {
		select {
		case <-r.auditStarted:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	verdict := `{"met":false,"reason":"Missing total"}`
	if r.calls > 1 {
		verdict = `{"met":true,"reason":"Total supplied"}`
	}
	return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: verdict}}}, nil
}

func (h *repairGraphStub) RepairGraphResult(_ context.Context, _ core.AgentTask, _ []core.TaskStep, _ core.AgentDeps, draft, feedback string) (string, error) {
	h.repairs++
	if draft != "Rejected report" || feedback != "Missing total" {
		return "", errors.New("lost persisted repair inputs")
	}
	if h.fail {
		return "", errors.New("repair provider unavailable")
	}
	return "Revised report", nil
}

func TestGraphFinalRepairResumesPersistedRejection(t *testing.T) {
	for _, mode := range []string{"success", "initial_rejection", "initial_rejection_audit", "validation_retry", "unavailable", "grounding", "partial_grounding", "ungrounded", "ungrounded_required", "ungrounded_review_exhausted", "exhausted"} {
		t.Run(mode, func(t *testing.T) {
			initialRejection := strings.HasPrefix(mode, "initial_rejection")
			db := finalizationDB(t)
			if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid DEFAULT gen_random_uuid(),ADD COLUMN cadence text`); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"026_agent_task_steps.sql", "027_agent_task_executor_version.sql", "028_agent_task_runs.sql"} {
				data, err := migrations.ReadFile("sql/" + name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(string(data)); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
			digest := sha256.Sum256([]byte("Original report"))
			state := map[string]any{"candidate_hash": hex.EncodeToString(digest[:]), "attempts": 1, "repair_pending": true, "response_checked": true, "safe_body": "Rejected report", "reason": "Missing total"}
			if mode == "grounding" {
				state["verdict"] = map[string]any{"grounding": map[string]any{"met": false}}
			}
			if mode == "partial_grounding" || strings.HasPrefix(mode, "ungrounded") {
				state["verdict"] = map[string]any{"grounding": map[string]any{"met": false, "claims": []map[string]any{{"claim": "Extra standard", "claim_type": "architectural", "status": "partial"}}}}
			}
			if strings.HasPrefix(mode, "ungrounded") {
				state["verdict"] = map[string]any{"grounding": map[string]any{"met": false, "claims": []map[string]any{{"claim": "Extra standard", "claim_type": "architectural", "status": "ungrounded"}}}}
			}
			if mode == "ungrounded_required" {
				state["repair_attempts"] = 1
			}
			if mode == "ungrounded_review_exhausted" {
				state["attempts"] = 3
			}
			if mode == "exhausted" {
				state["repair_attempts"] = 2
			}
			progress, _ := json.Marshal(map[string]any{"final_validation": state})
			if initialRejection {
				progress = json.RawMessage(`{}`)
			}
			if initialRejection || (mode == "partial_grounding" || strings.HasPrefix(mode, "ungrounded")) {
				if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN acceptance_criteria text; UPDATE agent_tasks SET acceptance_criteria='Include total'`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2,strategy='direct',deadline=now()+interval '10 minutes',progress=$2 WHERE id=$1`, id, progress); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,status,result) VALUES($1,'final','Report','Complete','finalize','done','Original report')`, id); err != nil {
				t.Fatal(err)
			}
			checks := 0
			cfg := &core.Config{ResponseValidator: func(_ context.Context, req core.ResponseValidationRequest) (string, error) {
				checks++
				if initialRejection && checks == 1 {
					if req.Text != "Original report" {
						t.Error(req.Text)
					}
					return "Rejected report", nil
				}
				if req.Text != "Revised report" {
					t.Error("repair bypassed validation", req.Text)
				}
				if mode == "validation_retry" && checks == 1 {
					return "", errors.New("validator unavailable")
				}
				return "Checked revision", nil
			}}
			cfg.ApplyDefaults()
			store := core.NewAgentTaskStore(db)
			reviewer := &repairAcceptanceStub{}
			if mode == "partial_grounding" || strings.HasPrefix(mode, "ungrounded") {
				reviewer.partialAudit = true
				if mode != "ungrounded_required" {
					reviewer.calls = 1
				}
			}
			if mode == "initial_rejection_audit" {
				reviewer.auditStarted = make(chan struct{})
			}
			cfg.Models.Primary.Name = "test-model"
			cfg.LLM = reviewer
			cfg.DB = os.Getenv("BLUESHIP_TEST_POSTGRES_DSN")
			if err := db.Get(&cfg.ShipSchema, `SELECT current_schema()`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE agent_task_tool_outputs(task_id uuid,tool_name text,tool_input jsonb,output text,output_format text,metadata jsonb,iteration int,created_at timestamptz)`); err != nil {
				t.Fatal(err)
			}
			if mode == "initial_rejection_audit" || (mode == "partial_grounding" || strings.HasPrefix(mode, "ungrounded")) {
				if _, err := db.Exec(`INSERT INTO agent_task_tool_outputs VALUES($1,'browser_fetch','{"url":"https://example.com/source"}','Revised report','html','{"final_url":"https://example.com/source"}',1,now())`, id); err != nil {
					t.Fatal(err)
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
			}
			deps, err := core.InitDeps(cfg, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(deps.Close)
			deps.Prompts = core.NewMapPromptStore(map[string]string{"background-graph-acceptance": "Review graph result"})
			deps.EnsureAutonomousHistory = func(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }
			scheduler := agenttask.NewScheduler(store, nil, nil, core.NewToolRegistry(), nil, deps, nil, slog.New(slog.DiscardHandler))
			handler := &repairGraphStub{fail: mode == "unavailable"}
			scheduler.SetGraphHandler(handler)
			if err := scheduler.Run(ctx); err != nil {
				t.Fatal(err)
			}
			scheduler.Wait()
			if mode == "unavailable" || mode == "validation_retry" {
				task, err := store.Get(ctx, id)
				if err != nil || task.Status != "pending" || handler.repairs != 1 {
					t.Fatal(task, err, handler)
				}
				if _, err := db.Exec(`UPDATE agent_task_runs SET next_attempt_at=now()-interval '1 second' WHERE task_id=$1`, id); err != nil {
					t.Fatal(err)
				}
				if err := scheduler.Run(ctx); err != nil {
					t.Fatal(err)
				}
				scheduler.Wait()
			}
			task, err := store.Get(ctx, id)
			if err != nil || task.Result == nil || handler.planned != 0 || handler.executed != 0 {
				t.Fatal("replayed graph or lost result", task, err, handler)
			}
			wantRepairs := 0
			if mode == "ungrounded_required" {
				wantRepairs = 1
				if task.Status != "done" || taskOutcome(task) != "partial" || *task.Result != "Checked revision" || checks != 1 || reviewer.calls != 1 {
					t.Fatal("deleting an unsupported required fact bypassed original acceptance", task, checks, reviewer.calls)
				}
			} else if mode == "success" || mode == "validation_retry" || (mode == "partial_grounding" || strings.HasPrefix(mode, "ungrounded")) || initialRejection {
				wantRepairs = 1
				wantChecks := 1
				if mode == "validation_retry" || initialRejection {
					wantChecks = 2
				}
				if task.Status != "done" || taskOutcome(task) != "completed" || *task.Result != "Checked revision" || checks != wantChecks {
					t.Fatal(task, checks)
				}
			} else {
				if mode == "unavailable" {
					wantRepairs = 2
				}
				if task.Status != "done" || taskOutcome(task) != "partial" || task.ErrorMessage != nil || *task.Result != "Rejected report" || checks != 0 {
					t.Fatal("invalid repair accepted", task, checks)
				}
			}
			if mode == "ungrounded_review_exhausted" {
				var saved struct {
					Final struct {
						Attempts int `json:"attempts"`
					} `json:"final_validation"`
				}
				if err := json.Unmarshal(task.Progress, &saved); err != nil || saved.Final.Attempts != 1 {
					t.Fatal("new candidate inherited exhausted review count", saved, err)
				}
			}
			if handler.repairs != wantRepairs {
				t.Fatal("unbounded or unsafe repair", handler.repairs, wantRepairs)
			}
			if initialRejection && reviewer.calls != 2 {
				t.Fatal("repair skipped acceptance", reviewer.calls)
			}
			if (mode == "partial_grounding" || strings.HasPrefix(mode, "ungrounded")) && reviewer.audits.Load() != 1 {
				t.Fatal("partly supported revision bypassed fresh audit", reviewer.audits.Load())
			}
			if mode == "initial_rejection_audit" && reviewer.audits.Load() != 2 {
				t.Fatal("repaired candidate was not audited anew", reviewer.audits.Load())
			}
		})
	}
}

type schedulerGraphStub struct{ planned, executed int }

func (*schedulerGraphStub) DefaultTools() []string { return nil }
func (h *schedulerGraphStub) PlanGraph(ctx context.Context, _ core.AgentTask, _ core.AgentDeps, _ string) ([]core.TaskStep, error) {
	if !core.DeferredProviderRetries(ctx) {
		return nil, errors.New("scheduler did not own planning retries")
	}
	h.planned++
	return []core.TaskStep{{ID: "final", Goal: "Answer", Acceptance: "Complete answer", Kind: "finalize"}}, nil
}
func (h *schedulerGraphStub) ExecuteGraphStep(ctx context.Context, task core.AgentTask, step core.TaskStep, inputs map[string]string, deps core.AgentDeps, save func(context.Context, json.RawMessage) error) (core.IterationResult, bool, error) {
	if !core.DeferredProviderRetries(ctx) {
		return core.IterationResult{}, false, errors.New("scheduler did not own step retries")
	}
	h.executed++
	if err := save(ctx, json.RawMessage(`{"phase":"verified","candidate_ready":true,"output":"Verified report"}`)); err != nil {
		return core.IterationResult{}, false, err
	}
	return core.IterationResult{Output: "Verified report"}, true, nil
}

func TestGraphSchedulerDispatchesV2AndFinalizes(t *testing.T) {
	db := finalizationDB(t)
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid DEFAULT gen_random_uuid(),ADD COLUMN cadence text, ADD COLUMN description text, ADD COLUMN acceptance_criteria text, ADD COLUMN delegate_to text, ADD COLUMN tools text[] NOT NULL DEFAULT '{}', ADD COLUMN use_agents text[] NOT NULL DEFAULT '{}', ADD COLUMN session_id text`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"026_agent_task_steps.sql", "027_agent_task_executor_version.sql", "028_agent_task_runs.sql", "031_agent_task_origin.sql"} {
		migration, err := migrations.ReadFile("sql/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	store := core.NewAgentTaskStore(db)
	user, soul := uuid.New(), uuid.New()
	cfg := &core.Config{BackgroundTasks: core.BackgroundTaskConfig{ExecutorV2: true, CanaryUserIDs: []string{user.String()}}}
	spec := core.AgentTask{UserID: user, Title: "Real creation path", Strategy: core.StrategyDirect, Handler: "background"}
	spec.ExecutorVersion = cfg.BackgroundTasks.ExecutorVersion(spec)
	origin := core.TaskOrigin{Transport: "telegram", BotID: uuid.New(), ChatID: "42"}
	created, err := store.Create(core.ContextWithTaskOrigin(core.WithSoulID(ctx, soul), origin), spec)
	if err != nil || created.ExecutorVersion != 2 {
		t.Fatal(created, err)
	}
	id := created.ID
	storedOrigin, present, originErr := core.TaskOriginFromConfig(created.Config)
	if originErr != nil || !present || storedOrigin != origin {
		t.Fatal("origin was not captured", storedOrigin, originErr)
	}
	if _, err := db.Exec(`UPDATE agent_tasks SET config=jsonb_set(config,'{delivery_origin,chat_id}','"99"') WHERE id=$1`, id); err == nil {
		t.Fatal("task origin was redirected")
	}
	if _, err := db.Exec(`UPDATE agent_tasks SET config=config || '{"other_setting":true}'::jsonb WHERE id=$1`, id); err != nil {
		t.Fatal("ordinary config update rejected", err)
	}
	// Disabling new creation must not strand the persisted v2 task.
	cfg.BackgroundTasks.ExecutorV2 = false
	cfg.ApplyDefaults()
	deps := &core.Deps{Config: cfg, EnsureAutonomousHistory: func(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }}
	scheduler := agenttask.NewScheduler(store, nil, nil, core.NewToolRegistry(), nil, deps, nil, slog.New(slog.DiscardHandler))
	handler := &schedulerGraphStub{}
	scheduler.SetGraphHandler(handler)
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	task, err := store.Get(ctx, id)
	if err != nil || task.Status != "done" || task.Result == nil || *task.Result != "Verified report" || handler.planned != 1 || handler.executed != 1 {
		t.Fatal(task, handler, err)
	}
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	if handler.planned != 1 || handler.executed != 1 {
		t.Fatal("terminal graph replayed", handler)
	}
	// One-shot graph tasks run to completion: a deadline stored by an older
	// host must not stop them. Time-bounded tasks still expire with their work.
	oneShot := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2,strategy='direct',status='paused',deadline=now()-interval '1 second' WHERE id=$1`, oneShot); err != nil {
		t.Fatal(err)
	}
	if due, err := store.ExpiredGraphTasks(ctx, time.Now(), 100); err != nil || len(due) != 0 {
		t.Fatal("one-shot graph task expired by a stored deadline", due, err)
	}
	for _, state := range []string{"pending", "running", "paused"} {
		expired := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
		if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2,strategy='recurring',status=$2,deadline=now()-interval '1 second' WHERE id=$1`, expired, state); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,status,result) VALUES($1,'read','Prices','Verified','read','done','Confirmed prices')`, expired); err != nil {
			t.Fatal(err)
		}
		if err := scheduler.Run(ctx); err != nil {
			t.Fatal(err)
		}
		scheduler.Wait()
		ended, err := store.Get(ctx, expired)
		if err != nil || ended.Status != "failed" || ended.Result == nil || *ended.Result != "Prices\nConfirmed prices" {
			t.Fatal("expiry lost useful work", state, ended, err)
		}
		if handler.planned != 1 || handler.executed != 1 {
			t.Fatal("expired graph executed", handler)
		}
		if _, changed, err := store.FinalizeTask(ctx, expired, core.TaskFinalization{Outcome: "partial", Body: "late", ExpiredAt: func() *time.Time { now := time.Now(); return &now }()}); err != nil || changed {
			t.Fatal("expiry repeated terminal write", changed, err)
		}
	}

}

type validationGraphStub struct{ schedulerGraphStub }

func (h *validationGraphStub) PlanGraph(context.Context, core.AgentTask, core.AgentDeps, string) ([]core.TaskStep, error) {
	h.planned++
	return []core.TaskStep{{ID: "read", Goal: "Read", Acceptance: "Evidence", Kind: "read"}, {ID: "final", Goal: "Report", Acceptance: "Confirmed", Kind: "finalize", Dependencies: []string{"read"}}}, nil
}
func (h *validationGraphStub) ExecuteGraphStep(ctx context.Context, task core.AgentTask, step core.TaskStep, inputs map[string]string, deps core.AgentDeps, save func(context.Context, json.RawMessage) error) (core.IterationResult, bool, error) {
	h.executed++
	receipts := []core.ToolExecutionResult{}
	if step.Kind == "read" {
		receipts = append(receipts, core.ToolExecutionResult{Name: "browser_fetch", Input: json.RawMessage(`{"url":"https://example.com/source"}`), Output: strings.Repeat("evidence ", 200)})
	}
	cp, _ := json.Marshal(map[string]any{"phase": "verified", "candidate_ready": true, "output": "Unfiltered report", "receipts": receipts})
	if err := save(ctx, cp); err != nil {
		return core.IterationResult{}, false, err
	}
	return core.IterationResult{Output: "Unfiltered report"}, true, nil
}

func TestGraphFinalValidationRetriesSavedResultWithAllBranchReceipts(t *testing.T) {
	db := finalizationDB(t)
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid DEFAULT gen_random_uuid(),ADD COLUMN cadence text`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"026_agent_task_steps.sql", "027_agent_task_executor_version.sql", "028_agent_task_runs.sql"} {
		data, err := migrations.ReadFile("sql/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2,strategy='direct',deadline=now()+interval '10 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	checks := 0
	cfg := &core.Config{ResponseValidator: func(_ context.Context, request core.ResponseValidationRequest) (string, error) {
		checks++
		if request.Text != "Unfiltered report" || len(request.Tools) != 1 || request.Tools[0].Output != strings.Repeat("evidence ", 200) {
			t.Error("branch receipts missing/truncated", request)
		}
		if checks == 1 {
			return "", core.NewHTTPFailure(429, "90", errors.New("validation provider unavailable"))
		}
		return "Validated report", nil
	}}
	cfg.ApplyDefaults()
	store := core.NewAgentTaskStore(db)
	scheduler := agenttask.NewScheduler(store, nil, nil, core.NewToolRegistry(), nil, &core.Deps{Config: cfg, EnsureAutonomousHistory: func(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }}, nil, slog.New(slog.DiscardHandler))
	handler := &validationGraphStub{}
	scheduler.SetGraphHandler(handler)
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	task, err := store.Get(ctx, id)
	if err != nil || task.Status != "pending" || task.Result != nil || checks != 1 {
		t.Fatal("unvalidated result published", task, err, checks)
	}
	var delay float64
	if err := db.Get(&delay, `SELECT extract(epoch FROM (next_attempt_at-now())) FROM agent_task_runs WHERE task_id=$1`, id); err != nil || delay < 85 {
		t.Fatal("provider Retry-After was ignored", delay, err)
	}
	if _, err := db.Exec(`UPDATE agent_task_runs SET next_attempt_at=now()-interval '1 second' WHERE task_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	task, err = store.Get(ctx, id)
	if err != nil || task.Status != "done" || task.Result == nil || *task.Result != "Validated report" || checks != 2 || handler.planned != 1 || handler.executed != 2 {
		t.Fatal("validation repeated graph or lost safe body", task, err, checks, handler)
	}
	var notification string
	// The notice is derived from the validated body only: heading, summary, report button.
	if err := db.Get(&notification, `SELECT message_text FROM agent_task_notification_attempts WHERE task_id=$1`, id); err != nil || notification != core.TaskReportNotification("Done: %s", task.Title, "Validated report", id) {
		t.Fatal("outbox contains unvalidated report", notification, err)
	}
}

// A rejected synthesis must retain access to the finalization reserve after
// research has ended; otherwise even a correctable report becomes terminal.
type rejectedFinalizerStub struct{ schedulerGraphStub }

func (h *rejectedFinalizerStub) ExecuteGraphStep(_ context.Context, _ core.AgentTask, _ core.TaskStep, _ map[string]string, _ core.AgentDeps, _ func(context.Context, json.RawMessage) error) (core.IterationResult, bool, error) {
	h.executed++
	return core.IterationResult{Output: "Needs the remaining table rows"}, false, nil
}

func TestGraphCandidateRetryUsesFinalizationReserve(t *testing.T) {
	for _, kind := range []string{"finalize", "read"} {
		t.Run(kind, func(t *testing.T) {
			db := finalizationDB(t)
			if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid DEFAULT gen_random_uuid(), ADD COLUMN cadence text`); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"026_agent_task_steps.sql", "027_agent_task_executor_version.sql", "028_agent_task_runs.sql"} {
				data, err := migrations.ReadFile("sql/" + name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(string(data)); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
			if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2,strategy='direct',created_at=now()-interval '8 minutes',deadline=now()+interval '1 minute' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,status,result) VALUES ($1,'read','Sources','Verified','read','done','All sources'),($1,'report','Report','Complete','finalize','pending','')`, id); err != nil {
				t.Fatal(err)
			}
			if kind == "read" {
				if _, err := db.Exec(`UPDATE agent_task_steps SET kind='read',checkpoint='{"phase":"rejected","candidate_ready":true,"output":"Candidate with a correctable fact"}' WHERE task_id=$1 AND step_id='report'`, id); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &core.Config{}
			cfg.ApplyDefaults()
			store := core.NewAgentTaskStore(db)
			scheduler := agenttask.NewScheduler(store, nil, nil, core.NewToolRegistry(), nil, &core.Deps{Config: cfg}, nil, slog.New(slog.DiscardHandler))
			h := &rejectedFinalizerStub{}
			scheduler.SetGraphHandler(h)
			if err := scheduler.Run(ctx); err != nil {
				t.Fatal(err)
			}
			scheduler.Wait()
			task, err := store.Get(ctx, id)
			if err != nil || task.Status != "pending" || task.Result != nil || h.executed != 1 {
				t.Fatal("finalizer lost reserved repair time", task, err, h.executed)
			}
			var next time.Time
			if err := db.Get(&next, `SELECT next_attempt_at FROM agent_task_runs WHERE task_id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if !next.After(time.Now()) || !next.Before(*task.Deadline) {
				t.Fatal("repair not scheduled inside finalization reserve", next, task.Deadline)
			}
		})
	}
}

type ownedReportStub struct {
	schedulerGraphStub
	t *testing.T
}

func (h *ownedReportStub) ExecuteGraphStep(_ context.Context, _ core.AgentTask, _ core.TaskStep, _ map[string]string, deps core.AgentDeps, _ func(context.Context, json.RawMessage) error) (core.IterationResult, bool, error) {
	if !deps.FinalAcceptanceOwned {
		h.t.Error("scheduler did not own final acceptance")
	}
	h.executed++
	return core.IterationResult{Output: "Unverified report"}, true, nil
}
func TestGraphAssembledReportCannotBypassFinalAcceptance(t *testing.T) {
	db := finalizationDB(t)
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid DEFAULT gen_random_uuid(), ADD COLUMN cadence text, ADD COLUMN acceptance_criteria text`); err != nil {
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
	if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2,strategy='direct',acceptance_criteria='Include total',deadline=now()+interval '10 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	cfg := &core.Config{}
	cfg.ApplyDefaults()
	cfg.Models.Primary.Name = "test-model"
	reviewer := &repairAcceptanceStub{}
	store := core.NewAgentTaskStore(db)
	cfg.LLM = reviewer
	cfg.DB = os.Getenv("BLUESHIP_TEST_POSTGRES_DSN")
	if err := db.Get(&cfg.ShipSchema, `SELECT current_schema()`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE agent_task_tool_outputs(task_id uuid,tool_name text,tool_input jsonb,output text,output_format text,metadata jsonb,iteration int,created_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	deps, err := core.InitDeps(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deps.Close)
	deps.Prompts = core.NewMapPromptStore(map[string]string{"background-graph-acceptance": "Review original task"})
	deps.EnsureAutonomousHistory = func(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }
	scheduler := agenttask.NewScheduler(store, nil, nil, core.NewToolRegistry(), nil, deps, nil, slog.New(slog.DiscardHandler))
	h := &ownedReportStub{t: t}
	scheduler.SetGraphHandler(h)
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	task, err := store.Get(ctx, id)
	if err != nil || task.Status != "done" || taskOutcome(task) != "partial" || task.Result == nil || reviewer.calls != 1 || h.executed != 1 {
		t.Fatal("unverified assembly became success", task, err, reviewer.calls, h.executed)
	}
	var progress struct {
		Outcome string `json:"result_outcome"`
	}
	if err := json.Unmarshal(task.Progress, &progress); err != nil {
		t.Fatal(err)
	}
	if progress.Outcome != "partial" {
		t.Fatal("rejected draft not preserved as partial", string(task.Progress))
	}
}

// taskOutcome is the finalized outcome. A one-shot graph report is always a
// "done" row; acceptance is recorded as completed or partial here.
func taskOutcome(task core.AgentTask) string {
	var progress struct {
		Outcome string `json:"result_outcome"`
	}
	_ = json.Unmarshal(task.Progress, &progress)
	return progress.Outcome
}
