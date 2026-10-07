package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/rasimio/blueship/internal/core"
)

func finalizationDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("BLUESHIP_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BLUESHIP_TEST_POSTGRES_DSN")
	}
	db := newAgentTaskSchema(t, dsn)
	for _, name := range []string{"017_agent_task_deliveries.sql", "018_agent_task_notification_journal.sql", "024_agent_task_checkpoints.sql", "025_agent_task_artifacts.sql", "026_agent_task_steps.sql", "028_agent_task_runs.sql", "032_agent_task_step_progress.sql", "033_agent_task_dismissals.sql"} {
		data, err := migrations.ReadFile("sql/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE agent_task_iterations (task_id uuid, iteration integer, output text)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestFinalizationAtomicallySavesDraftAndQueuesDelivery(t *testing.T) {
	db := finalizationDB(t)
	ctx := context.Background()
	store := core.NewAgentTaskStore(db)
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	deadline := time.Now().Add(-time.Second)
	if _, err := db.Exec(`UPDATE agent_tasks SET deadline=$2 WHERE id=$1`, id, deadline); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agent_task_iterations VALUES ($1,1,$2)`, id, "<scratchpad>internal audit</scratchpad>\nVerified component prices\n<<<EVIDENCE_JSON [] >>>\n[CONTINUE]"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	artifact, changed, err := store.FinalizeTask(ctx, id, core.TaskFinalization{Outcome: "partial", Reason: core.TaskDeadlineExceeded, ExpiredAt: &now, EmptyBody: "no usable result", Notify: "partial result available"})
	if err != nil || !changed || artifact.Outcome != "partial" || artifact.Body != "Verified component prices" {
		t.Fatalf("artifact=%+v changed=%v err=%v", artifact, changed, err)
	}
	task, err := store.Get(ctx, id)
	if err != nil || task.Status != "failed" || task.Result == nil || *task.Result != artifact.Body {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	var state string
	var attempts int
	if err := db.QueryRow(`SELECT state,attempt_count FROM agent_task_notification_attempts WHERE task_id=$1`, id).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "retryable" || attempts != 0 {
		t.Fatalf("outbox=%s attempts=%d", state, attempts)
	}
	if _, changed, err := store.FinalizeTask(ctx, id, core.TaskFinalization{Outcome: "partial", Body: "duplicate", Notify: "duplicate"}); err != nil || changed {
		t.Fatalf("repeated finalize: %v %v", changed, err)
	}
	var count int
	if err := db.Get(&count, `SELECT count(*) FROM agent_task_artifacts WHERE task_id=$1`, id); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	// A terminal artifact can be delivered later without rerunning its task.
	intent, err := store.ClaimRetryableNotification(ctx, time.Now().Add(time.Second))
	if err != nil || intent == nil || intent.TaskID != id || intent.Text != "partial result available" {
		t.Fatalf("intent=%+v err=%v", intent, err)
	}
}

func TestFinalizationOutboxFailureRollsBackTerminalState(t *testing.T) {
	db := finalizationDB(t)
	ctx := context.Background()
	store := core.NewAgentTaskStore(db)
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if _, err := db.Exec(`ALTER TABLE agent_task_notification_attempts ADD CHECK (message_text <> 'reject-test')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.FinalizeTask(ctx, id, core.TaskFinalization{Outcome: "completed", Body: "verified report", Notify: "reject-test"}); err == nil {
		t.Fatal("expected outbox failure")
	}
	task, err := store.Get(ctx, id)
	if err != nil || task.Status != "pending" || task.Result != nil {
		t.Fatalf("partial commit: %+v %v", task, err)
	}
	var count int
	if err := db.Get(&count, `SELECT count(*) FROM agent_task_artifacts WHERE task_id=$1`, id); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestFinalizationRequiresCurrentClaimAndExplainsEmptyWork(t *testing.T) {
	db := finalizationDB(t)
	ctx := context.Background()
	store := core.NewAgentTaskStore(db)
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	if ok, err := store.TrySetRunning(ctx, id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	task, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	stale := task.LastRunAt.Add(-time.Second)
	if _, _, err := store.FinalizeTask(ctx, id, core.TaskFinalization{Outcome: "completed", Body: "stale", ClaimStartedAt: &stale}); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatal(err)
	}
	artifact, changed, err := store.FinalizeTask(ctx, id, core.TaskFinalization{Outcome: "partial", EmptyBody: "No sources could be reached.", Notify: "draft exists", EmptyNotify: "No sources could be reached.", ClaimStartedAt: task.LastRunAt})
	if err != nil || !changed || artifact.Outcome != "blocked" || artifact.Body != "No sources could be reached." {
		t.Fatalf("%+v %v %v", artifact, changed, err)
	}
	var notification string
	if err := db.Get(&notification, `SELECT message_text FROM agent_task_notification_attempts WHERE task_id=$1`, id); err != nil || notification != "No sources could be reached." {
		t.Fatal(notification, err)
	}
}

func TestFinalizationQueuesCompletedBodyWhenNotificationOmitted(t *testing.T) {
	db := finalizationDB(t)
	ctx := context.Background()
	store := core.NewAgentTaskStore(db)
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{}`))
	artifact, changed, err := store.FinalizeTask(ctx, id, core.TaskFinalization{Outcome: "completed", Body: "Verified deliverable", Notify: "  "})
	if err != nil || !changed {
		t.Fatalf("finalization: %v %v", changed, err)
	}
	intent, err := store.ClaimRetryableNotification(ctx, time.Now().Add(time.Second))
	if err != nil || intent == nil || intent.TaskID != id || intent.Text != artifact.Body {
		t.Fatalf("missing delivery: %+v %v", intent, err)
	}
}

func TestFinalizationPreservesRejectedReadDraftWithoutPromotingIt(t *testing.T) {
	for _, kind := range []string{"read", "mixed", "repair", "action", "inflight"} {
		t.Run(kind, func(t *testing.T) {
			db := finalizationDB(t)
			for _, name := range []string{"026_agent_task_steps.sql", "027_agent_task_executor_version.sql"} {
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
			deadline := time.Now().Add(-time.Minute)
			if _, err := db.Exec(`UPDATE agent_tasks SET executor_version=2,deadline=$2 WHERE id=$1`, id, deadline); err != nil {
				t.Fatal(err)
			}
			ready := kind != "inflight" && kind != "repair"
			stepKind := kind
			if stepKind == "inflight" || stepKind == "mixed" || stepKind == "repair" {
				stepKind = "read"
			}
			cp, _ := json.Marshal(map[string]any{"candidate_ready": ready, "output": "<scratchpad>PRIVATE</scratchpad>Six priced items https://example.com/source", "review_reason": "Missing case and cooler; over budget"})
			if kind == "repair" {
				cp = json.RawMessage(`{"candidate_ready":false,"output":"INTERNAL repair narration","last_candidate":"Six priced items https://example.com/source","last_candidate_review":"over budget"}`)
			}

			if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,status,checkpoint) VALUES($1,'research','Build','Complete',$2,'blocked',$3)`, id, stepKind, cp); err != nil {
				t.Fatal(err)
			}
			if kind == "mixed" {
				if _, err := db.Exec(`INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,status,result) VALUES($1,'fx','Rate','Verified','read','done','Confirmed exchange rate')`, id); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			artifact, changed, err := core.NewAgentTaskStore(db).FinalizeTask(ctx, id, core.TaskFinalization{Outcome: "partial", ExpiredAt: &now, EmptyBody: "No usable draft", EmptyNotify: "No draft", Notify: "Partial saved", UnverifiedDraftFmt: "NOT VERIFIED: %s\n%s\nGAPS: %s"})
			if err != nil || !changed {
				t.Fatal(artifact, changed, err)
			}
			if kind == "read" || kind == "mixed" || kind == "repair" {
				if kind == "mixed" && !strings.Contains(artifact.Body, "Confirmed exchange rate") {
					t.Fatal("lost confirmed result", artifact)
				}
				if artifact.Outcome != "partial" || !strings.Contains(artifact.Body, "NOT VERIFIED") || !strings.Contains(artifact.Body, "Six priced items") || !strings.Contains(artifact.Body, "over budget") || strings.Contains(artifact.Body, "PRIVATE") || strings.Contains(artifact.Body, "INTERNAL") {
					t.Fatal(artifact)
				}
			} else if artifact.Outcome != "blocked" || artifact.Body != "No usable draft" {
				t.Fatal("published in-flight text or action claim", artifact)
			}
		})
	}
}
