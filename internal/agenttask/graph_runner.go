package agenttask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

// GraphHandler is implemented by the runtime's background LLM adapter.
type GraphHandler interface {
	DefaultTools() []string
	PlanGraph(context.Context, core.AgentTask, core.AgentDeps, string) ([]core.TaskStep, error)
	ExecuteGraphStep(context.Context, core.AgentTask, core.TaskStep, map[string]string, core.AgentDeps, func(context.Context, json.RawMessage) error) (core.IterationResult, bool, error)
}

func (s *Scheduler) SetGraphHandler(handler GraphHandler) {
	s.SetGraphRunner(func(ctx context.Context, task core.AgentTask) error { return s.executeGraphTask(ctx, task, handler) })
}

func (s *Scheduler) graphDeps(ctx context.Context, task core.AgentTask, h GraphHandler) (core.AgentDeps, error) {
	registry := s.registry
	if s.registryBuilder != nil {
		registry = s.registryBuilder(s.deps.ForUser(task.UserID, "agent_task:"+task.ID.String(), false))
	}
	requested, hasRequested, err := taskRequestedTools(task)
	if err != nil {
		return core.AgentDeps{}, err
	}
	registry, err = s.composeSoulTaskRegistry(ctx, task, registry, requested)
	if err != nil {
		return core.AgentDeps{}, err
	}
	registry = registryForTask(registry, h.DefaultTools(), requested, hasRequested)
	if hasRequested && registry.Count() == 0 {
		return core.AgentDeps{}, fmt.Errorf("task has no permitted tools")
	}
	return core.AgentDeps{FinalAcceptanceOwned: task.AcceptanceCriteria != nil && strings.TrimSpace(*task.AcceptanceCriteria) != "", LLM: s.deps.LLM, Embedder: s.deps.Embedder, Registry: registry, RoleTools: s.deps.RoleTools, ModelStore: s.deps.ModelStore, Store: s.msgStore, Prompts: s.deps.Prompts, Users: s.deps.Users, Sessions: s.deps.Sessions, Logger: s.logger, DB: s.deps.DB, UserID: task.UserID, Config: s.deps.Config, Deliveries: s.store, ContextInjector: s.deps.ContextInjector}, nil
}

func (s *Scheduler) executeGraphTask(ctx context.Context, task core.AgentTask, h GraphHandler) error {
	ctx = core.WithDeferredProviderRetries(ctx)
	deps, err := s.graphDeps(ctx, task, h)
	if err != nil {
		return s.finishGraph(ctx, task, "partial", "", "tool_configuration_failed")
	}
	researchUntil := core.TaskResearchDeadline(task, s.deps.Config.Timeouts.TaskFinalizeReserve)
	steps, err := s.store.TaskSteps(ctx, task.ID)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		if researchUntil != nil && !time.Now().Before(*researchUntil) {
			return s.finishGraph(ctx, task, "partial", "", "research_budget_exhausted")
		}
		planCtx := ctx
		if researchUntil != nil {
			var cancel context.CancelFunc
			planCtx, cancel = context.WithDeadline(ctx, *researchUntil)
			defer cancel()
		}

		var progress struct {
			PlanAttempts    int    `json:"plan_attempts"`
			ContextPrepared bool   `json:"context_prepared"`
			ContextSnapshot string `json:"context_snapshot"`
		}
		if err := json.Unmarshal(task.Progress, &progress); err != nil {
			return err
		}
		if !progress.ContextPrepared {
			if deps.ContextInjector != nil {
				progress.ContextSnapshot = deps.ContextInjector(planCtx, task.UserID.String(), task.Title, "")
			}
			progress.ContextPrepared = true
			raw, _ := json.Marshal(progress)
			if err := s.store.SaveGraphProgress(ctx, task.ID, raw); err != nil {
				return err
			}
		}
		progress.PlanAttempts++
		progressJSON, _ := json.Marshal(progress)
		if err := s.store.SaveGraphProgress(ctx, task.ID, progressJSON); err != nil {
			return err
		}
		steps, err = h.PlanGraph(planCtx, task, deps, progress.ContextSnapshot)
		if err != nil {
			s.logger.WarnContext(ctx, "agent-tasks: graph planning failed", "task_id", task.ID, "attempt", progress.PlanAttempts, "error", err)
			if retry, delay := core.TaskRetryPolicy(err); retry && progress.PlanAttempts < 3 {
				next := time.Now().Add(max(delay, time.Duration(progress.PlanAttempts)*5*time.Second))
				if researchUntil == nil || next.Before(*researchUntil) {
					return s.store.YieldGraphTask(ctx, task.ID, next)
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Never stop at planning: a single research branch and the report
			// still deliver a result.
			s.logger.WarnContext(ctx, "agent-tasks: using fallback graph plan", "task_id", task.ID)
			steps = fallbackGraphPlan(deps.Registry)
		}
		if err := s.store.SaveTaskSteps(ctx, task.ID, *task.LastRunAt, steps); err != nil {
			return err
		}
	}
	deps.TaskStepGoals = make(map[string]string, len(steps))
	for _, planned := range steps {
		deps.TaskStepGoals[planned.ID] = planned.Goal
	}
	executor := GraphExecutor{Store: s.store, ParallelReads: core.BackgroundParallelReads, Lease: 30 * time.Second, ResearchDeadline: researchUntil, SeparateReadBudget: true}
	runErr := executor.Run(ctx, task.ID, func(ctx context.Context, step core.TaskStep, inputs map[string]string, checkpoint func(context.Context, json.RawMessage) error) (StepResult, error) {
		result, accepted, err := h.ExecuteGraphStep(ctx, task, step, inputs, deps, checkpoint)
		if retry, delay := core.TaskRetryPolicy(err); retry {
			return StepResult{Output: result.Output}, &RetryableStepError{Cause: err, Delay: delay}
		}
		if errors.Is(err, core.ErrTaskVerificationUnavailable) {
			return StepResult{Output: result.Output}, &RetryableStepError{Cause: err}
		}
		if err == nil && !accepted && (step.Kind == "read" || (step.Kind == "finalize" && len(step.Tools) == 0)) {
			return StepResult{Output: result.Output}, &RetryableStepError{Cause: fmt.Errorf("step acceptance rejected")}
		}
		return StepResult{Output: result.Output, Accepted: accepted}, err
	})
	if errors.Is(runErr, core.ErrTaskClaimLost) || ctx.Err() != nil {
		return runErr
	}
	steps, err = s.store.TaskSteps(ctx, task.ID)
	if err != nil {
		return err
	}
	if errors.Is(runErr, ErrGraphResearchBudget) {
		return s.finishGraphPartial(ctx, task, steps, deps, h, "research_budget_exhausted")
	}
	stuck := false
	for _, step := range steps {
		if step.Status == "blocked" || step.Status == "reconciliation" {
			if step.Kind == "finalize" {
				return s.finishGraphPartial(ctx, task, steps, deps, h, "finalize_"+step.Status)
			}
			stuck = true
		}
	}
	if stuck {
		// A stuck branch never stops the task: its best output and the gap go
		// on to the report.
		if _, err := s.store.ResolveStuckTaskSteps(ctx, task.ID); err != nil {
			return err
		}
		return s.executeGraphTask(ctx, task, h)
	}
	var next time.Time
	var output string
	allDone := true
	researchPending := false
	statuses := map[string]string{}
	for _, step := range steps {
		statuses[step.ID] = step.Status
	}
	for _, step := range steps {
		switch step.Status {
		case "done":
			if step.Kind == "finalize" {
				output = step.Result
			}
		default:
			allDone = false
			if step.Kind != "finalize" && !core.TaskStepHasCandidate(step) {
				researchPending = true
			}
			ready := step.Status == "pending"
			for _, dep := range step.Dependencies {
				if statuses[dep] != "done" {
					ready = false
				}
			}
			if ready && (next.IsZero() || step.NextAttemptAt.Before(next)) {
				next = step.NextAttemptAt
			}
		}
	}
	if allDone && strings.TrimSpace(output) != "" {
		return s.validateGraphResult(ctx, task, steps, deps, output, h)
	}
	if next.IsZero() {
		return s.finishGraphPartial(ctx, task, steps, deps, h, "graph_incomplete")
	}
	// Never retain a compute slot while waiting for a known retry. Recovery of
	// cancelled sibling reads uses their bounded leases before yielding.
	if next.Before(time.Now().Add(5 * time.Second)) {
		next = time.Now().Add(5 * time.Second)
	}
	if researchPending && researchUntil != nil && !next.Before(*researchUntil) {
		return s.finishGraphPartial(ctx, task, steps, deps, h, "research_budget_exhausted")
	}
	return s.store.YieldGraphTask(ctx, task.ID, next)
}

func (s *Scheduler) finishGraph(ctx context.Context, task core.AgentTask, outcome, body, reason string) error {
	finishCtx, end := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer end()
	// Every finished report gets the same notice and "open report" button;
	// its limitations are stated inside the report, not as a failure message.
	notify := core.TaskReportNotification(s.deps.Config.UI.TaskReadyFmt, task.Title, body, task.ID)
	empty := fmt.Sprintf(s.deps.Config.UI.TaskFailureEmptyFmt, task.Title)
	artifact, changed, err := s.store.FinalizeTask(finishCtx, task.ID, core.TaskFinalization{Outcome: outcome, Body: body, Reason: reason, Notify: notify, UnverifiedDraftFmt: s.deps.Config.UI.UnverifiedDraftFmt, EmptyBody: empty, EmptyNotify: empty})
	if err != nil || !changed {
		return err
	}
	result := core.IterationResult{Output: artifact.Body, IsFinal: true, Partial: artifact.Outcome != "completed"}
	if s.deps.AgentIterationCompletedHook != nil {
		s.deps.AgentIterationCompletedHook(finishCtx, task, result)
	}
	s.afterArtifactFinalized(finishCtx, task, result, core.TaskTerminalStatus(task, artifact.Outcome))
	return nil
}

func (s *Scheduler) finishGraphPartial(ctx context.Context, task core.AgentTask, steps []core.TaskStep, deps core.AgentDeps, h GraphHandler, reason string) error {
	var body string
	if synthesizer, ok := h.(interface {
		SynthesizeGraphPartial(context.Context, core.AgentTask, []core.TaskStep, core.AgentDeps) (string, error)
	}); ok {
		finalCtx, end := context.WithTimeout(ctx, 2*time.Minute)
		draft, err := synthesizer.SynthesizeGraphPartial(finalCtx, task, steps, deps)
		end()
		if err == nil {
			body = draft
		}
	}
	return s.finishGraph(ctx, task, "partial", body, reason)
}

// fallbackGraphPlan keeps a task moving when the planner cannot produce a
// valid graph: one research branch with the task's read-only tools, then the
// report.
func fallbackGraphPlan(registry *core.ToolRegistry) []core.TaskStep {
	var tools []string
	for _, def := range registry.Definitions() {
		if registry.IsReadOnly(def.Name) {
			tools = append(tools, def.Name)
		}
	}
	return []core.TaskStep{
		{ID: "research", Kind: "read", Goal: "Research everything the task asks for, within its stated sources and constraints.", Acceptance: "Every requirement of the task is covered with exact sources; data that could not be found is stated explicitly.", Tools: tools, MaxAttempts: 3},
		{ID: "report", Kind: "finalize", Goal: "Write the requested final deliverable from the research.", Acceptance: "All task requirements are addressed; unsupported claims and missing data are explicit.", Dependencies: []string{"research"}, MaxAttempts: 3},
	}
}
