package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/core"
)

// A final repair does not replace the graph step's accepted draft. Deadline
// maintenance must preserve the newer response-checked report in task progress,
// without treating an interrupted final audit as a successful task.
func TestGraphDeadlinePreservesLatestCheckedFinalCandidate(t *testing.T) {
	const original = "Original report: board dimensions are unavailable."
	const corrected = "Corrected report: board dimensions are 244 by 226 mm."
	for _, mode := range []string{"checked", "unchecked", "empty", "internal_only", "missing", "invalid_guard"} {
		t.Run(mode, func(t *testing.T) {
			db := finalizationDB(t)
			if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid DEFAULT gen_random_uuid(), ADD COLUMN cadence text`); err != nil {
				t.Fatal(err)
			}
			migration, err := migrations.ReadFile("sql/027_agent_task_executor_version.sql")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(string(migration)); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
			final := map[string]any{
				"response_checked": true, "safe_body": "<scratchpad>PRIVATE</scratchpad>" + corrected,
				"repair_body": "UNVALIDATED REPAIR", "repair_attempts": 2,
				"accepted": false, "verdict": map[string]any{"met": false, "unavailable": true},
			}
			want := original
			switch mode {
			case "checked":
				want = corrected
			case "unchecked":
				final["response_checked"] = false
			case "empty":
				final["safe_body"] = " \n "
			case "internal_only":
				final["safe_body"] = "<scratchpad>PRIVATE</scratchpad>"
			case "missing":
				final = nil
			case "invalid_guard":
				final["response_checked"] = "true"
			}
			progress, err := json.Marshal(map[string]any{"phase": "finalizing", "final_validation": final})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2, strategy='recurring', status='running', deadline=now()-interval '1 second', progress=$2 WHERE id=$1`, id, progress); err != nil {
				t.Fatal(err)
			}
			checkpoint, err := json.Marshal(map[string]any{"candidate_ready": true, "output": original})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,status,result,checkpoint) VALUES($1,'final','Report','Complete','finalize','done',$2,$3)`, id, original, checkpoint); err != nil {
				t.Fatal(err)
			}
			cfg := &core.Config{}
			cfg.ApplyDefaults()
			store := core.NewAgentTaskStore(db)
			deps := &core.Deps{Config: cfg}
			scheduler := agenttask.NewScheduler(store, nil, nil, core.NewToolRegistry(), nil, deps, nil, slog.New(slog.DiscardHandler))
			handler := &schedulerGraphStub{}
			scheduler.SetGraphHandler(handler)
			for range 2 {
				if err := scheduler.Run(ctx); err != nil {
					t.Fatal(err)
				}
				scheduler.Wait()
			}
			task, err := store.Get(ctx, id)
			if err != nil || task.Status != "failed" || task.Result == nil || *task.Result != want || task.ErrorMessage == nil || *task.ErrorMessage != core.TaskDeadlineExceeded {
				t.Fatal("deadline lost latest safe report or promoted incomplete audit", task, err)
			}
			var artifact core.TaskArtifact
			if err := db.Get(&artifact, `SELECT * FROM agent_task_artifacts WHERE task_id=$1`, id); err != nil || artifact.Outcome != "partial" || artifact.Body != want || artifact.Reason != core.TaskDeadlineExceeded || artifact.Version != 1 {
				t.Fatal("incorrect partial artifact", artifact, err)
			}
			var terminal struct {
				Outcome string `json:"result_outcome"`
				Final   struct {
					Accepted bool `json:"accepted"`
				} `json:"final_validation"`
			}
			if err := json.Unmarshal(task.Progress, &terminal); err != nil || terminal.Outcome != "partial" || terminal.Final.Accepted {
				t.Fatal("incomplete audit promoted in progress", string(task.Progress), err)
			}
			if handler.planned != 0 || handler.executed != 0 {
				t.Fatal("deadline resumed graph execution", handler)
			}
			var notifications int
			if err := db.Get(&notifications, `SELECT count(*) FROM agent_task_notification_attempts WHERE task_id=$1`, id); err != nil || notifications != 1 {
				t.Fatal("deadline duplicated notification", notifications, err)
			}
		})
	}
}

type guardedFinalRepairStub struct {
	schedulerGraphStub
	repairs int
}

func (h *guardedFinalRepairStub) RepairGraphResult(_ context.Context, _ core.AgentTask, _ []core.TaskStep, _ core.AgentDeps, draft, _ string) (string, error) {
	h.repairs++
	if draft != "Previously checked repaired report" {
		return "", errors.New("repair lost previous checked report")
	}
	return "Unchecked next repair", nil
}

func TestGraphFinalGuardInterruptionRetainsPreviousCheckedRepair(t *testing.T) {
	for _, mode := range []string{"deadline_during_guard", "guard_exhaustion", "already_exhausted", "no_prior_guard", "no_prior_exhausted"} {
		t.Run(mode, func(t *testing.T) {
			db := finalizationDB(t)
			if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid DEFAULT gen_random_uuid(), ADD COLUMN cadence text`); err != nil {
				t.Fatal(err)
			}
			migration, err := migrations.ReadFile("sql/027_agent_task_executor_version.sql")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(string(migration)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
			const original = "Original report"
			const checked = "Previously checked repaired report"
			digest := sha256.Sum256([]byte(original))
			final := map[string]any{
				"candidate_hash": hex.EncodeToString(digest[:]), "response_checked": true,
				"safe_body": checked, "repair_body": checked, "repair_attempts": 1,
				"repair_pending": true, "reason": "Another factual correction required",
			}
			want, wantRepairs, wantChecks := checked, 1, 1
			if mode == "guard_exhaustion" {
				wantChecks = 3
			}
			if mode == "already_exhausted" || mode == "no_prior_guard" || mode == "no_prior_exhausted" {
				final["response_checked"], final["safe_body"] = false, ""
				final["repair_pending"], final["repair_body"] = false, "Unchecked next repair"
				final["last_checked_body"], final["attempts"] = checked, 3
				wantRepairs, wantChecks = 0, 0
			}
			if mode == "no_prior_guard" || mode == "no_prior_exhausted" {
				delete(final, "last_checked_body")
				want = original
			}
			if mode == "no_prior_guard" {
				final["attempts"], wantChecks = 2, 1
			}
			progress, _ := json.Marshal(map[string]any{"final_validation": final})
			if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2, strategy='recurring', deadline=now()+interval '10 minutes', progress=$2 WHERE id=$1`, id, progress); err != nil {
				t.Fatal(err)
			}
			checkpoint, _ := json.Marshal(map[string]any{"candidate_ready": true, "output": original})
			if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,status,result,checkpoint) VALUES($1,'final','Report','Complete','finalize','done',$2,$3)`, id, original, checkpoint); err != nil {
				t.Fatal(err)
			}
			guardStarted := make(chan struct{})
			checks := 0
			cfg := &core.Config{ResponseValidator: func(ctx context.Context, req core.ResponseValidationRequest) (string, error) {
				checks++
				if req.Text != "Unchecked next repair" {
					t.Error("guard received wrong candidate", req.Text)
				}
				if mode == "deadline_during_guard" {
					close(guardStarted)
					<-ctx.Done()
					return "", ctx.Err()
				}
				return "", errors.New("output guard unavailable")
			}}
			cfg.ApplyDefaults()
			store := core.NewAgentTaskStore(db)
			deps := &core.Deps{Config: cfg, EnsureAutonomousHistory: func(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }}
			scheduler := agenttask.NewScheduler(store, nil, nil, core.NewToolRegistry(), nil, deps, nil, slog.New(slog.DiscardHandler))
			handler := &guardedFinalRepairStub{}
			scheduler.SetGraphHandler(handler)
			t.Cleanup(func() { cancel(); scheduler.Wait() })
			if err := scheduler.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if mode == "deadline_during_guard" {
				select {
				case <-guardStarted:
				case <-time.After(3 * time.Second):
					t.Fatal("repair never reached output guard")
				}
				// Inspect the actual persisted transition before expiry: the newest
				// repair is unchecked, while the earlier guarded report survives.
				var last string
				var checkedNow bool
				if err := db.QueryRow(`SELECT progress->'final_validation'->>'last_checked_body', (progress->'final_validation'->>'response_checked')::boolean FROM agent_tasks WHERE id=$1`, id).Scan(&last, &checkedNow); err != nil || last != checked || checkedNow {
					t.Fatal("repair transition dropped checked predecessor", last, checkedNow, err)
				}
				// Move the persisted deadline instead of making the test wait the
				// production repair admission reserve of thirty seconds.
				if _, err := db.Exec(`UPDATE agent_tasks SET deadline=now()-interval '1 second' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
				if err := scheduler.Run(context.Background()); err != nil {
					t.Fatal(err)
				}
				cancel()
			}
			scheduler.Wait()
			if mode == "guard_exhaustion" {
				for range 2 {
					if _, err := db.Exec(`UPDATE agent_task_runs SET next_attempt_at=now()-interval '1 second' WHERE task_id=$1`, id); err != nil {
						t.Fatal(err)
					}
					if err := scheduler.Run(ctx); err != nil {
						t.Fatal(err)
					}
					scheduler.Wait()
				}
			}
			task, err := store.Get(context.Background(), id)
			if err != nil || task.Status != "failed" || task.Result == nil || *task.Result != want || handler.repairs != wantRepairs || checks != wantChecks || handler.planned != 0 || handler.executed != 0 {
				t.Fatal("guard interruption lost safe draft, published unchecked repair or repeated graph work", task, handler, checks, err)
			}
			var artifact core.TaskArtifact
			if err := db.Get(&artifact, `SELECT * FROM agent_task_artifacts WHERE task_id=$1`, id); err != nil || artifact.Body != want || artifact.Outcome != "partial" || artifact.Version != 1 {
				t.Fatal("unsafe or promoted artifact", artifact, err)
			}
			var terminal struct {
				Outcome string `json:"result_outcome"`
				Final   struct {
					Accepted bool `json:"accepted"`
				} `json:"final_validation"`
			}
			if err := json.Unmarshal(task.Progress, &terminal); err != nil || terminal.Outcome != "partial" || terminal.Final.Accepted {
				t.Fatal("unchecked repair promoted", string(task.Progress), err)
			}
		})
	}
}
