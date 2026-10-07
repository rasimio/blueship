package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/core"
)

func graphFixture(t *testing.T, steps []core.TaskStep) (*core.AgentTaskStore, uuid.UUID) {
	t.Helper()
	db := finalizationDB(t)
	data, err := migrations.ReadFile("sql/026_agent_task_steps.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatal(err)
	}
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
	if err := store.SaveTaskSteps(ctx, id, *task.LastRunAt, steps); err != nil {
		t.Fatal(err)
	}
	return store, id
}

func TestGraphExecutorParallelReadsHeartbeatAndRestart(t *testing.T) {
	store, id := graphFixture(t, []core.TaskStep{
		{ID: "a", Goal: "Read a", Acceptance: "Verified a", Kind: "read"},
		{ID: "b", Goal: "Read b", Acceptance: "Verified b", Kind: "read"},
		{ID: "report", Goal: "Write report", Acceptance: "Saved document", Kind: "action", Dependencies: []string{"a", "b"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan string, 2)
	release := make(chan struct{})
	finished := make(chan error, 1)
	var active, peak atomic.Int32
	exec := agenttask.GraphExecutor{Store: store, ParallelReads: 2, Lease: time.Second}
	go func() {
		finished <- exec.Run(ctx, id, func(ctx context.Context, step core.TaskStep, inputs map[string]string, checkpoint func(context.Context, json.RawMessage) error) (agenttask.StepResult, error) {
			if step.Kind == "read" {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				if err := checkpoint(ctx, json.RawMessage(`{"saved":"evidence"}`)); err != nil {
					return agenttask.StepResult{}, err
				}
				started <- step.ID
				select {
				case <-release:
				case <-ctx.Done():
					return agenttask.StepResult{}, ctx.Err()
				}
				return agenttask.StepResult{Output: step.ID + " verified", Accepted: true}, nil
			}
			if active.Load() != 0 || inputs["a"] != "a verified" || inputs["b"] != "b verified" {
				return agenttask.StepResult{}, fmt.Errorf("action started without confirmed inputs: %v", inputs)
			}
			return agenttask.StepResult{Output: "Saved report receipt", Accepted: true}, nil
		})
	}()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("independent reads did not overlap")
		}
	}
	// Wait for a real heartbeat, not just a live goroutine or checkpoint write.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		steps, err := store.TaskSteps(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		renewed := 0
		for _, step := range steps {
			if step.Kind == "read" && step.HeartbeatAt != nil && step.StartedAt != nil && step.HeartbeatAt.Sub(*step.StartedAt) > 200*time.Millisecond {
				var cp map[string]string
				if err := json.Unmarshal(step.Checkpoint, &cp); err != nil || cp["saved"] != "evidence" {
					t.Fatal("heartbeat erased checkpoint", string(step.Checkpoint), err)
				}
				renewed++
			}
		}
		if renewed == 2 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("heartbeat not persisted")
		}
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("parallelism=%d", peak.Load())
	}
	calls := 0
	if err := exec.Run(ctx, id, func(context.Context, core.TaskStep, map[string]string, func(context.Context, json.RawMessage) error) (agenttask.StepResult, error) {
		calls++
		return agenttask.StepResult{}, fmt.Errorf("unexpected replay")
	}); err != nil || calls != 0 {
		t.Fatal("restarted completed work", calls, err)
	}
}

func TestGraphExecutorRejectsUnverifiedResultWithoutReleasingDependent(t *testing.T) {
	store, id := graphFixture(t, []core.TaskStep{
		{ID: "read", Goal: "Fetch price", Acceptance: "Confirmed price", Kind: "read"},
		{ID: "report", Goal: "Save report", Acceptance: "Report receipt", Kind: "action", Dependencies: []string{"read"}},
	})
	exec := agenttask.GraphExecutor{Store: store, ParallelReads: 2, Lease: time.Second}
	calls := 0
	err := exec.Run(context.Background(), id, func(_ context.Context, step core.TaskStep, _ map[string]string, _ func(context.Context, json.RawMessage) error) (agenttask.StepResult, error) {
		calls++
		return agenttask.StepResult{Output: "Unverified price", Accepted: false}, nil
	})
	if err == nil || calls != 1 {
		t.Fatal("acceptance ignored", calls, err)
	}
	steps, err := store.TaskSteps(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.ID == "read" && step.Status != "blocked" {
			t.Fatal(step.Status)
		}
		if step.ID == "report" && step.Attempts != 0 {
			t.Fatal("dependent ran")
		}
	}
}

func TestGraphClaimsExcludeActionsAcrossExecutorInstances(t *testing.T) {
	store, id := graphFixture(t, []core.TaskStep{
		{ID: "action", Goal: "Send document", Acceptance: "Delivery receipt", Kind: "action"},
		{ID: "read", Goal: "Fetch source", Acceptance: "Verified source", Kind: "read"},
	})
	ctx := context.Background()
	action, err := store.ClaimTaskStep(ctx, id, "action", time.Minute)
	if err != nil || action == nil {
		t.Fatal(action, err)
	}
	// This claim represents another executor, with no shared in-memory state.
	if read, err := store.ClaimTaskStep(ctx, id, "read", time.Minute); err != nil || read != nil {
		t.Fatal("read overlapped foreign action", read, err)
	}
	if err := store.FailTaskStep(ctx, id, "action", *action.RunID, "delivery outcome unknown", true); err != nil {
		t.Fatal(err)
	}
	if read, err := store.ClaimTaskStep(ctx, id, "read", time.Minute); err != nil || read != nil {
		t.Fatal("unresolved action allowed more work", read, err)
	}
}

func TestGraphResearchBudgetStopsWorkerAndPreservesFinalization(t *testing.T) {
	store, id := graphFixture(t, []core.TaskStep{{ID: "read", Goal: "Read", Acceptance: "Verified", Kind: "read"}, {ID: "final", Goal: "Report", Acceptance: "Complete", Kind: "finalize", Dependencies: []string{"read"}}})
	deadline := time.Now().Add(150 * time.Millisecond)
	executor := agenttask.GraphExecutor{Store: store, Lease: time.Second, ParallelReads: 2, ResearchDeadline: &deadline}
	calls := 0
	err := executor.Run(context.Background(), id, func(ctx context.Context, step core.TaskStep, _ map[string]string, _ func(context.Context, json.RawMessage) error) (agenttask.StepResult, error) {
		calls++
		<-ctx.Done()
		return agenttask.StepResult{}, ctx.Err()
	})
	if !errors.Is(err, agenttask.ErrGraphResearchBudget) || calls != 1 {
		t.Fatal("research did not stop", calls, err)
	}
	steps, err := store.TaskSteps(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.Status == "running" {
			t.Fatal("worker not drained", step.ID)
		}
		if step.Kind == "finalize" && step.Attempts != 0 {
			t.Fatal("incomplete report marked done")
		}
	}
}

func TestGraphReadRetryPersistsProviderDelay(t *testing.T) {
	store, id := graphFixture(t, []core.TaskStep{{ID: "read", Goal: "Read", Acceptance: "Verified", Kind: "read"}})
	ctx := context.Background()
	step, err := store.ClaimTaskStep(ctx, id, "read", time.Minute)
	if err != nil || step == nil {
		t.Fatal(step, err)
	}
	before := time.Now()
	if err := store.FailTaskStepAfter(ctx, id, "read", *step.RunID, "rate limited", true, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	steps, err := store.TaskSteps(ctx, id)
	if err != nil || len(steps) != 1 || steps[0].Status != "pending" || steps[0].NextAttemptAt.Before(before.Add(90*time.Second)) {
		t.Fatal(steps, err)
	}
	if retry, err := store.ClaimTaskStep(ctx, id, "read", time.Minute); err != nil || retry != nil {
		t.Fatal("ignored provider retry delay", retry, err)
	}
}

func TestGraphDeferredRetryPreservesIndependentWorkers(t *testing.T) {
	store, id := graphFixture(t, []core.TaskStep{
		{ID: "a", Goal: "Review source", Acceptance: "Verified", Kind: "read"},
		{ID: "b", Goal: "Read another source", Acceptance: "Verified", Kind: "read"},
		{ID: "c", Goal: "Read queued source", Acceptance: "Verified", Kind: "read"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bStarted := make(chan struct{})
	cStarted := make(chan struct{})
	executor := agenttask.GraphExecutor{Store: store, ParallelReads: 2, Lease: time.Second}
	err := executor.Run(ctx, id, func(ctx context.Context, step core.TaskStep, _ map[string]string, _ func(context.Context, json.RawMessage) error) (agenttask.StepResult, error) {
		switch step.ID {
		case "a":
			select {
			case <-bStarted:
			case <-ctx.Done():
				return agenttask.StepResult{}, ctx.Err()
			}
			return agenttask.StepResult{}, &agenttask.RetryableStepError{Cause: core.ErrTaskVerificationUnavailable, Delay: time.Minute}
		case "b":
			close(bStarted)
			select {
			case <-cStarted:
			case <-ctx.Done():
				return agenttask.StepResult{}, ctx.Err()
			}
		case "c":
			close(cStarted)
		}
		return agenttask.StepResult{Accepted: true, Output: step.ID + " verified"}, nil
	})
	if !errors.Is(err, agenttask.ErrGraphWaiting) {
		t.Fatal(err)
	}
	steps, err := store.TaskSteps(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		want := "done"
		if step.ID == "a" {
			want = "pending"
			if time.Until(step.NextAttemptAt) < 50*time.Second {
				t.Fatal("retry delay not preserved")
			}
		}
		if step.Status != want || step.Attempts != 1 {
			t.Fatalf("independent work canceled or replayed: %+v", step)
		}
	}
}

func TestGraphRetryWakesWhileSiblingRemainsActive(t *testing.T) {
	store, id := graphFixture(t, []core.TaskStep{
		{ID: "a", Goal: "Retry source", Acceptance: "Verified", Kind: "read"},
		{ID: "b", Goal: "Long source", Acceptance: "Verified", Kind: "read"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	bStarted := make(chan struct{})
	retried := make(chan struct{})
	executor := agenttask.GraphExecutor{Store: store, ParallelReads: 2, Lease: time.Second}
	err := executor.Run(ctx, id, func(ctx context.Context, step core.TaskStep, _ map[string]string, _ func(context.Context, json.RawMessage) error) (agenttask.StepResult, error) {
		if step.ID == "a" {
			if step.Attempts == 1 {
				select {
				case <-bStarted:
				case <-ctx.Done():
					return agenttask.StepResult{}, ctx.Err()
				}
				return agenttask.StepResult{}, &agenttask.RetryableStepError{Cause: core.ErrTaskVerificationUnavailable}
			}
			close(retried)
		} else {
			close(bStarted)
			select {
			case <-retried:
			case <-ctx.Done():
				return agenttask.StepResult{}, ctx.Err()
			}
		}
		return agenttask.StepResult{Accepted: true, Output: "verified"}, nil
	})
	if err != nil {
		t.Fatal("ready retry waited for unrelated worker", err)
	}
	steps, err := store.TaskSteps(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		want := 1
		if step.ID == "a" {
			want = 2
		}
		if step.Status != "done" || step.Attempts != want {
			t.Fatal("retry/peer state", step)
		}
	}
}

func TestGraphSeparateReadBudgetKeepsVerificationAlive(t *testing.T) {
	store, id := graphFixture(t, []core.TaskStep{{ID: "read", Goal: "Read", Acceptance: "Verified", Kind: "read"}, {ID: "final", Goal: "Report", Acceptance: "Complete", Kind: "finalize", Dependencies: []string{"read"}}})
	deadline := time.Now().Add(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	executor := agenttask.GraphExecutor{Store: store, Lease: time.Second, ParallelReads: 2, ResearchDeadline: &deadline, SeparateReadBudget: true}
	calls := 0
	err := executor.Run(ctx, id, func(ctx context.Context, step core.TaskStep, _ map[string]string, save func(context.Context, json.RawMessage) error) (agenttask.StepResult, error) {
		calls++
		if step.Kind == "read" {
			if err := save(ctx, json.RawMessage(`{"phase":"verification","candidate_ready":true,"output":"Evidence"}`)); err != nil {
				return agenttask.StepResult{}, err
			}
			timer := time.NewTimer(time.Until(deadline) + 50*time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return agenttask.StepResult{}, ctx.Err()
			}
		}
		return agenttask.StepResult{Output: "Verified", Accepted: true}, nil
	})
	if err != nil || calls != 2 {
		t.Fatal("verification/finalization lost reserve", err, calls)
	}
}

func TestGraphFinalizerProviderTimeoutDoesNotBecomeResearchExpiry(t *testing.T) {
	store, id := graphFixture(t, []core.TaskStep{{ID: "report", Goal: "Report", Acceptance: "Complete", Kind: "finalize"}})
	expired := time.Now().Add(-time.Minute)
	executor := agenttask.GraphExecutor{Store: store, Lease: time.Second, ParallelReads: 2, ResearchDeadline: &expired, SeparateReadBudget: true}
	err := executor.Run(context.Background(), id, func(context.Context, core.TaskStep, map[string]string, func(context.Context, json.RawMessage) error) (agenttask.StepResult, error) {
		return agenttask.StepResult{}, &agenttask.RetryableStepError{Cause: fmt.Errorf("provider timeout: %w", context.DeadlineExceeded), Delay: 10 * time.Second}
	})
	if !errors.Is(err, agenttask.ErrGraphWaiting) || errors.Is(err, agenttask.ErrGraphResearchBudget) {
		t.Fatal("provider timeout became research expiry", err)
	}
	steps, err := store.TaskSteps(context.Background(), id)
	if err != nil || len(steps) != 1 || steps[0].Status != "pending" || steps[0].Attempts != 1 || !steps[0].NextAttemptAt.After(time.Now()) {
		t.Fatal(steps, err)
	}
}

func TestGraphSharesOnlyConfirmedReadResultsWithinTask(t *testing.T) {
	store, id := graphFixture(t, []core.TaskStep{
		{ID: "fx", Kind: "read", Goal: "Exchange rate", Acceptance: "Confirmed rate"},
		{ID: "old_action", Kind: "action", Goal: "Authorized previous action", Acceptance: "Receipt"},
		{ID: "baseline", Kind: "read", Goal: "Affordable combination", Acceptance: "Within cap"},
		{ID: "detail", Kind: "read", Goal: "Verify chosen items", Acceptance: "Verified", Dependencies: []string{"baseline"}},
		{ID: "effect", Kind: "action", Goal: "Authorized action", Acceptance: "Receipt", Dependencies: []string{"baseline"}},
		{ID: "final", Kind: "finalize", Goal: "Report", Acceptance: "Complete", Dependencies: []string{"fx", "old_action", "detail", "effect"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, stepID := range []string{"fx", "old_action"} {
		step, err := store.ClaimTaskStep(ctx, id, stepID, time.Second)
		if err != nil || step == nil || step.RunID == nil {
			t.Fatal(step, err)
		}
		if err := store.CompleteTaskStep(ctx, id, stepID, *step.RunID, stepID+" confirmed"); err != nil {
			t.Fatal(err)
		}
	}
	executed := map[string]bool{}
	executor := agenttask.GraphExecutor{Store: store, ParallelReads: 1, Lease: time.Second}
	err := executor.Run(ctx, id, func(_ context.Context, step core.TaskStep, inputs map[string]string, _ func(context.Context, json.RawMessage) error) (agenttask.StepResult, error) {
		executed[step.ID] = true
		switch step.ID {
		case "baseline":
			if len(inputs) != 1 || inputs["fx"] != "fx confirmed" {
				return agenttask.StepResult{}, fmt.Errorf("missing completed sibling or leaked unconfirmed/effect data: %v", inputs)
			}
		case "detail":
			if inputs["baseline"] != "baseline confirmed" || inputs["fx"] != "fx confirmed" {
				return agenttask.StepResult{}, fmt.Errorf("missing dependency/sibling: %v", inputs)
			}
			if _, ok := inputs["old_action"]; ok {
				return agenttask.StepResult{}, errors.New("unrelated action result injected into read")
			}
		case "effect":
			if len(inputs) != 1 || inputs["baseline"] != "baseline confirmed" {
				return agenttask.StepResult{}, fmt.Errorf("action received undeclared data: %v", inputs)
			}
		case "final":
			if inputs["baseline"] != "baseline confirmed" || inputs["fx"] != "fx confirmed" || inputs["effect"] != "effect confirmed" || inputs["old_action"] != "old_action confirmed" {
				return agenttask.StepResult{}, fmt.Errorf("finalizer lost transitive read or explicit action receipt: %v", inputs)
			}
		default:
			return agenttask.StepResult{}, errors.New("completed step replayed")
		}
		return agenttask.StepResult{Accepted: true, Output: step.ID + " confirmed"}, nil
	})
	if err != nil || len(executed) != 4 {
		t.Fatal(executed, err)
	}
}
