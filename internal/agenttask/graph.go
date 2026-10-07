package agenttask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

// StepExecution receives confirmed dependency results and its saved checkpoint.
// Read/finalize steps also receive already-confirmed read results from this task;
// optional sibling evidence never changes dependency readiness or action inputs.
type StepExecution func(context.Context, core.TaskStep, map[string]string, func(context.Context, json.RawMessage) error) (StepResult, error)
type StepResult struct {
	Output   string
	Accepted bool
}

// RetryableStepError classifies a transient read failure. Unclassified errors
// do not silently consume the task lifetime with endless retries.
type RetryableStepError struct {
	Cause error
	Delay time.Duration
}

func (e *RetryableStepError) Error() string { return e.Cause.Error() }
func (e *RetryableStepError) Unwrap() error { return e.Cause }

var ErrGraphResearchBudget = errors.New("graph research budget exhausted")

var ErrGraphWaiting = errors.New("task graph is waiting for recovery, retry or reconciliation")

// GraphExecutor runs ready reads concurrently and external effects exclusively.
// It owns no polling loop: deferred retries return control to the dispatcher.
type GraphExecutor struct {
	ResearchDeadline *time.Time
	// SeparateReadBudget requires the handler to enforce the research deadline
	// on tools, allowing candidate synthesis/review within the task deadline.
	SeparateReadBudget bool
	Store              *core.AgentTaskStore
	ParallelReads      int
	Lease              time.Duration
}

func (g GraphExecutor) Run(ctx context.Context, taskID uuid.UUID, execute StepExecution) error {
	if g.Store == nil || execute == nil || g.ParallelReads < 1 || g.ParallelReads > 8 || g.Lease < time.Second || g.Lease > 10*time.Minute {
		return fmt.Errorf("invalid graph executor configuration")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type completion struct {
		kind string
		id   string
		err  error
	}
	completed := make(chan completion, g.ParallelReads)
	running := map[string]bool{}
	exclusive := false
	var stopped error
	for {
		var wakeAt time.Time
		if stopped == nil && ctx.Err() != nil {
			stopped = ctx.Err()
		}
		if stopped == nil {
			steps, err := g.Store.TaskSteps(ctx, taskID)
			if err != nil {
				stopped = err
			} else if len(steps) == 0 {
				stopped = fmt.Errorf("task has no persisted plan")
			} else {
				byID := make(map[string]core.TaskStep, len(steps))
				done := 0
				for _, step := range steps {
					byID[step.ID] = step
					if step.Status == "done" {
						done++
					}
				}
				if done == len(steps) && len(running) == 0 {
					return nil
				}
				for _, step := range steps {
					if exclusive || len(running) >= g.ParallelReads {
						break
					}
					if step.Status != "pending" || running[step.ID] {
						continue
					}
					if step.Kind != "read" && len(running) > 0 {
						continue
					}
					inputs := map[string]string{}
					ready := true
					for _, dep := range step.Dependencies {
						p, ok := byID[dep]
						if !ok || p.Status != "done" {
							ready = false
							break
						}
						inputs[dep] = p.Result
					}
					if !ready {
						continue
					}
					if step.Kind == "read" || step.Kind == "finalize" {
						for _, observed := range steps {
							if observed.Kind == "read" && observed.Status == "done" {
								inputs[observed.ID] = observed.Result
							}
						}
					}
					if step.NextAttemptAt.After(time.Now()) {
						if wakeAt.IsZero() || step.NextAttemptAt.Before(wakeAt) {
							wakeAt = step.NextAttemptAt
						}
						continue
					}
					if step.Kind != "finalize" && !(g.SeparateReadBudget && core.TaskStepHasCandidate(step)) && g.ResearchDeadline != nil && !time.Now().Before(*g.ResearchDeadline) {
						stopped = ErrGraphResearchBudget
						break
					}
					claimed, err := g.Store.ClaimTaskStep(ctx, taskID, step.ID, g.Lease)
					if err != nil {
						stopped = err
						break
					}
					if claimed == nil {
						continue
					}
					running[step.ID] = true
					exclusive = step.Kind != "read"
					go func(step core.TaskStep, inputs map[string]string) {
						completed <- completion{kind: step.Kind, id: step.ID, err: g.runStep(ctx, step, inputs, execute)}
					}(*claimed, inputs)
				}
			}
		}
		if stopped != nil {
			cancel()
		}
		if len(running) == 0 {
			if stopped != nil {
				return stopped
			}
			return ErrGraphWaiting
		}
		// Drain every worker before returning. A failed sibling cannot leave a
		// hidden goroutine issuing further external actions after finalization.
		var event completion
		if stopped != nil {
			event = <-completed
		} else {
			var timer *time.Timer
			var wake <-chan time.Time
			if !wakeAt.IsZero() && !exclusive && len(running) < g.ParallelReads {
				timer = time.NewTimer(time.Until(wakeAt))
				wake = timer.C
			}
			gotCompletion := false
			select {
			case event = <-completed:
				gotCompletion = true
			case <-wake:
			case <-ctx.Done():
			}
			if timer != nil {
				timer.Stop()
			}
			if !gotCompletion {
				continue
			}
		}
		delete(running, event.id)
		exclusive = false
		researchBound := event.kind != "finalize" && !(g.SeparateReadBudget && event.kind == "read")
		researchExpired := researchBound && g.ResearchDeadline != nil && !time.Now().Before(*g.ResearchDeadline)
		if errors.Is(event.err, ErrGraphWaiting) && ctx.Err() == nil && !(researchExpired && errors.Is(event.err, context.DeadlineExceeded)) {
			continue // Persisted deferred retry must not cancel independent workers.
		}
		if event.err != nil && stopped == nil {
			stopped = event.err
			if researchExpired && ctx.Err() == nil && errors.Is(event.err, context.DeadlineExceeded) {
				stopped = ErrGraphResearchBudget
			}
		}
	}
}

func (g GraphExecutor) runStep(ctx context.Context, step core.TaskStep, inputs map[string]string, execute StepExecution) error {
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	if step.Kind != "finalize" && !(g.SeparateReadBudget && step.Kind == "read") && g.ResearchDeadline != nil {
		cancel()
		ctx, cancel = context.WithDeadline(parentCtx, *g.ResearchDeadline)
	}
	defer cancel()
	heartbeatDone := make(chan error, 1)
	stopHeartbeat := make(chan struct{})
	go func() {
		ticker := time.NewTicker(g.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				heartbeatDone <- nil
				return
			case <-ctx.Done():
				heartbeatDone <- ctx.Err()
				return
			case <-ticker.C:
				persistCtx, end := context.WithTimeout(ctx, g.Lease/3)
				err := g.Store.CheckpointTaskStep(persistCtx, step.TaskID, step.ID, *step.RunID, g.Lease, nil)
				end()
				if err != nil {
					cancel()
					heartbeatDone <- err
					return
				}
			}
		}
	}()
	checkpoint := func(persistCtx context.Context, cp json.RawMessage) error {
		return g.Store.CheckpointTaskStep(persistCtx, step.TaskID, step.ID, *step.RunID, g.Lease, cp)
	}
	result, err := execute(ctx, step, inputs, checkpoint)
	close(stopHeartbeat)
	heartbeatErr := <-heartbeatDone
	if heartbeatErr != nil {
		if !errors.Is(heartbeatErr, core.ErrTaskClaimLost) {
			persistCtx, end := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			persistErr := g.Store.FailTaskStep(persistCtx, step.TaskID, step.ID, *step.RunID, heartbeatErr.Error(), true)
			end()
			if g.ResearchDeadline != nil && !time.Now().Before(*g.ResearchDeadline) && parentCtx.Err() == nil && !errors.Is(persistErr, core.ErrTaskClaimLost) {
				return errors.Join(ErrGraphResearchBudget, persistErr)
			}
			return errors.Join(heartbeatErr, persistErr)
		}
		return heartbeatErr
	}
	if err == nil && (!result.Accepted || strings.TrimSpace(result.Output) == "") {
		err = fmt.Errorf("step %s did not pass acceptance", step.ID)
	}
	if err == nil {
		return g.Store.CompleteTaskStep(ctx, step.TaskID, step.ID, *step.RunID, result.Output)
	}
	var retryable *RetryableStepError
	persistCtx, end := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer end()
	canRetry := errors.As(err, &retryable)
	delay := time.Duration(0)
	if canRetry {
		delay = retryable.Delay
	}
	if persistErr := g.Store.FailTaskStepAfter(persistCtx, step.TaskID, step.ID, *step.RunID, err.Error(), canRetry, delay); persistErr != nil {
		return errors.Join(err, persistErr)
	}
	if canRetry && step.Attempts < step.MaxAttempts {
		return errors.Join(ErrGraphWaiting, err)
	}
	return err
}
