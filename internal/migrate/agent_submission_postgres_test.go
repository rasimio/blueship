package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

func TestReviewRetryPreservesCandidateWithoutBurningResearchIteration(t *testing.T) {
	db := finalizationDB(t)
	ctx := context.Background()
	store := core.NewAgentTaskStore(db)
	id := insertPendingTask(t, ctx, db, "background", json.RawMessage(`{"session_id":"worker"}`))
	if ok, err := store.TrySetRunning(ctx, id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	task, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	result := core.IterationResult{Done: true, Output: "Complete candidate report", Notify: "report ready", Progress: task.Progress, ToolCallsJSON: json.RawMessage(`[{"name":"lookup","output":"confirmed"}]`), PendingDeliveries: []core.TaskDeliveryRef{{InputID: "payload", ItemKey: "one"}}}
	if err := store.SaveTaskSubmission(ctx, id, *task.LastRunAt, result); err != nil {
		t.Fatal(err)
	}
	next := time.Now().Add(time.Minute)
	if err := store.DeferTaskVerification(ctx, id, *task.LastRunAt, "reviewer unavailable", next); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, id)
	if err != nil || got.Status != "pending" || got.Iteration != task.Iteration {
		t.Fatalf("review consumed research: %+v %v", got, err)
	}
	sub, err := store.TaskSubmission(ctx, id)
	if err != nil || sub == nil || sub.Output != result.Output || sub.ReviewAttempts != 1 || sub.NextReviewAt.Sub(next) > time.Millisecond {
		t.Fatalf("submission=%+v err=%v", sub, err)
	}
	var refs []core.TaskDeliveryRef
	if err := json.Unmarshal(sub.PendingDeliveries, &refs); err != nil || len(refs) != 1 || refs[0].ItemKey != "one" {
		t.Fatal(refs, err)
	}
	if err := store.SaveTaskSubmission(ctx, id, *task.LastRunAt, result); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatalf("pending task accepted stale report: %v", err)
	}
	// Deadline salvage picks the actual submitted report even if no successful
	// iteration audit was written before the process stopped.
	deadline := time.Now().Add(-time.Second)
	if _, err := db.Exec(`UPDATE agent_tasks SET deadline=$2 WHERE id=$1`, id, deadline); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	artifact, changed, err := store.FinalizeTask(ctx, id, core.TaskFinalization{Outcome: "partial", Reason: "verification_unavailable", ExpiredAt: &now, EmptyBody: "nothing found", Notify: "candidate needs verification"})
	if err != nil || !changed || artifact.Body != result.Output || artifact.Outcome != "partial" {
		t.Fatalf("artifact=%+v changed=%v err=%v", artifact, changed, err)
	}
}

func TestReviewRejectionFencesStaleWorkerAndSchedulesRepair(t *testing.T) {
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
	candidate := core.IterationResult{Output: "candidate", Progress: json.RawMessage(`{}`)}
	if err := store.SaveTaskSubmission(ctx, id, *task.LastRunAt, candidate); err != nil {
		t.Fatal(err)
	}
	stale := task.LastRunAt.Add(-time.Second)
	if err := store.RejectTaskSubmission(ctx, id, stale, json.RawMessage(`{"reason":"stale"}`), nil); !errors.Is(err, core.ErrTaskClaimLost) {
		t.Fatal(err)
	}
	sub, err := store.TaskSubmission(ctx, id)
	if err != nil || sub == nil || sub.Output != "candidate" {
		t.Fatalf("lost candidate: %+v %v", sub, err)
	}
	current, err := store.Get(ctx, id)
	if err != nil || current.Status != "running" || current.Iteration != task.Iteration {
		t.Fatalf("stale rejection changed task: %+v %v", current, err)
	}
	if err := store.RejectTaskSubmission(ctx, id, *task.LastRunAt, json.RawMessage(`{"reason":"missing price"}`), []string{"https://example.com/price"}); err != nil {
		t.Fatal(err)
	}
	current, err = store.Get(ctx, id)
	if err != nil || current.Status != "pending" || current.Iteration != task.Iteration+1 || len(current.RequiredRecheckURLs) != 1 {
		t.Fatalf("repair not scheduled: %+v %v", current, err)
	}
	sub, err = store.TaskSubmission(ctx, id)
	if err != nil || sub != nil {
		t.Fatalf("rejected report still queued: %+v %v", sub, err)
	}
}
