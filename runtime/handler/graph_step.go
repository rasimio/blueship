package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

var ErrGraphVerificationUnavailable = core.ErrTaskVerificationUnavailable
var ErrGraphActionUncertain = errors.New("graph step action requires reconciliation")

type graphStepCheckpoint struct {
	LastCandidate       string                     `json:"last_candidate,omitempty"`
	LastCandidateReview string                     `json:"last_candidate_review,omitempty"`
	ReviewReason        string                     `json:"review_reason,omitempty"`
	SessionID           string                     `json:"session_id"`
	Phase               string                     `json:"phase"`
	Output              string                     `json:"output"`
	CandidateReady      bool                       `json:"candidate_ready"`
	ReviewAttempts      int                        `json:"review_attempts"`
	Traces              []agent.ToolTrace          `json:"traces"`
	Receipts            []core.ToolExecutionResult `json:"receipts"`
	PendingTool         *core.ContentBlock         `json:"pending_tool,omitempty"`
}

// ExecuteGraphStep has an isolated session and a hard tool ceiling. Saved
// candidates go directly to review, never replaying tools on reviewer outage.
func (b *Background) ExecuteGraphStep(ctx context.Context, task core.AgentTask, step core.TaskStep, inputs map[string]string, deps core.AgentDeps, save func(context.Context, json.RawMessage) error) (core.IterationResult, bool, error) {
	var empty core.IterationResult
	if deps.Prompts == nil || deps.LLM == nil || deps.Store == nil || deps.Registry == nil || deps.Config == nil || save == nil {
		return empty, false, fmt.Errorf("graph step dependencies unavailable")
	}
	otherGoals := make(map[string]string)
	for id, goal := range deps.TaskStepGoals {
		if id != step.ID {
			otherGoals[id] = goal
		}
	}
	cp := graphStepCheckpoint{}
	if len(step.Checkpoint) > 0 {
		if err := json.Unmarshal(step.Checkpoint, &cp); err != nil {
			return empty, false, fmt.Errorf("invalid step checkpoint: %w", err)
		}
	}
	if cp.PendingTool != nil && step.Kind != "read" {
		return empty, false, ErrGraphActionUncertain
	}
	model := deps.Config.Models.Primary
	if deps.ModelStore != nil {
		if ref := deps.ModelStore.Get(modelRoleForTask(task)); ref.Name != "" {
			model = ref
		}
	}
	if model.ForRouter() == "" {
		return empty, false, fmt.Errorf("graph step model unavailable")
	}
	persist := func(ctx context.Context) error {
		raw, err := json.Marshal(cp)
		if err != nil {
			return err
		}
		return save(ctx, raw)
	}
	result := func() core.IterationResult {
		traces, _ := json.Marshal(cp.Traces)
		return core.IterationResult{Output: cp.Output, ToolCallsJSON: traces}
	}
	if cp.Phase == "rejected" && (step.Kind == "read" || (step.Kind == "finalize" && len(step.Tools) == 0)) {
		if step.Kind == "read" && cp.CandidateReady && strings.TrimSpace(cp.Output) != "" {
			cp.LastCandidate, cp.LastCandidateReview = cp.Output, cp.ReviewReason
		}
		cp.CandidateReady = false
		cp.ReviewAttempts = 0
		cp.SessionID = "" // Repair starts from the candidate/feedback, not a stale compacted dialog.
	}
	if cp.CandidateReady && cp.Phase == "verified" {
		return result(), true, nil
	}
	if cp.CandidateReady && cp.ReviewAttempts >= 3 {
		return result(), false, ErrGraphVerificationUnavailable
	}
	if !cp.CandidateReady {
		previousTraces := append([]agent.ToolTrace(nil), cp.Traces...)
		previousReceipts := append([]core.ToolExecutionResult(nil), cp.Receipts...)
		allowed := append([]string{}, step.Tools...)
		definitions := deps.Registry.DefinitionsForNames(allowed)
		if len(definitions) != len(allowed) {
			return empty, false, fmt.Errorf("step tools no longer available")
		}
		for _, name := range allowed {
			if step.Kind == "read" && !deps.Registry.IsReadOnly(name) {
				return empty, false, fmt.Errorf("effectful tool %q in read step", name)
			}
			if step.Kind == "finalize" && deps.Registry.IsReadOnly(name) {
				return empty, false, fmt.Errorf("research tool %q in finalization", name)
			}
		}
		prompt, err := deps.Prompts.Get(ctx, "background-graph-step")
		if err != nil {
			return empty, false, err
		}
		if strings.TrimSpace(prompt) == "" {
			return empty, false, fmt.Errorf("background-graph-step prompt is empty")
		}
		if cp.SessionID == "" {
			cp.SessionID, err = deps.Store.CreateSessionWithSource(ctx, task.UserID.String(), model.Name, "agent_task", task.ID.String())
			if err != nil {
				return empty, false, err
			}
		}
		cp.Phase = "executing"
		if err := persist(ctx); err != nil {
			return result(), false, err
		}
		previousCandidate := cp.Output
		if step.Kind == "read" && cp.LastCandidate != "" {
			previousCandidate = cp.LastCandidate
		}
		input, err := json.Marshal(map[string]any{"current_datetime": b.graphCurrentDatetime(ctx, deps),
			"step_id": step.ID, "assigned_to_other_steps": otherGoals,
			"task": task.Title, "task_description": task.Description, "task_acceptance": task.AcceptanceCriteria,
			"kind": step.Kind, "goal": step.Goal, "acceptance": step.Acceptance, "confirmed_inputs": inputs,
			"previous_candidate": previousCandidate, "repair_feedback": cp.ReviewReason,
		})
		if err != nil {
			return result(), false, err
		}
		registry := deps.Registry.SubsetForNames(allowed)
		// Repair may inspect already saved evidence during the finalization
		// reserve. Replace handlers, not just tool descriptions: no network
		// fallback or external action is reachable through this registry.
		evidenceOnly := false
		if step.Kind == "read" {
			if deadline := core.TaskResearchDeadline(task, deps.Config.Timeouts.TaskFinalizeReserve); deadline != nil && !time.Now().Before(*deadline) {
				evidenceOnly = true
				registry = registry.EvidenceOnlySubset()
				allowed = nil
				for _, definition := range registry.Definitions() {
					allowed = append(allowed, definition.Name)
				}
				prompt += "\nExternal research is closed. Available tools can only read this task's saved evidence; a missing cached source cannot be fetched. Resolve only gaps supported by saved data and retain any unresolved requirements."
			}
		}
		loop := agent.NewLoop(deps.LLM, deps.Store, registry, deps.RoleTools, deps.Config, deps.Logger)
		loop.SetCompactor(newTaskCompactor(ctx, deps))
		parallelReads := 0
		if step.Kind == "read" {
			parallelReads = core.BackgroundParallelReads
		}
		var toolsDeadline time.Time
		if step.Kind == "read" && !evidenceOnly {
			if deadline := core.TaskResearchDeadline(task, deps.Config.Timeouts.TaskFinalizeReserve); deadline != nil {
				toolsDeadline = *deadline
			}
		}
		maxTurns := graphToolTurnLimit(deps)
		// Coordination reads assemble tables as large as a final report. At
		// effort high 4096 (+1 continuation) cut them mid-row; the reviewer
		// then rejected every attempt and the task stopped (production,
		// 2026-09-23). 16384 is the non-streamed ceiling, as for synthesis.
		maxTokens := 16384
		ctx = core.ContextWithIteration(core.ContextWithTaskID(ctx, task.ID), step.Attempts)
		out, runErr := loop.RunTracked(ctx, agent.RunConfig{SessionID: cp.SessionID, SystemPrompt: prompt, Model: model.ForRouter(), Role: modelRoleForTask(task), MaxTokens: maxTokens, MaxTurns: maxTurns + 1, MaxToolTurns: maxTurns, ToolsDeadline: toolsDeadline, ContextWindow: model.ContextWindow, ToolOverride: allowed, StrictTools: true, ParallelReadTools: parallelReads, Effort: model.Effort, ThinkingMode: model.ThinkingMode,
			OnCheckpoint: func(persistCtx context.Context, event agent.RunCheckpoint) error {
				previousOperation := cp.PendingTool
				cp.Phase = event.Phase
				cp.Output = event.Text
				cp.Traces = append(append([]agent.ToolTrace(nil), previousTraces...), event.ToolTraces...)
				cp.PendingTool = event.PendingTool
				cp.Receipts = append([]core.ToolExecutionResult(nil), previousReceipts...)
				for _, trace := range event.ToolTraces {
					if trace.Receipt != nil {
						cp.Receipts = append(cp.Receipts, *trace.Receipt)
					}
				}
				// A tool error does not prove that an external mutation failed.
				// Preserve its identity and stop even the remaining tools in
				// this model response; a prompt warning cannot enforce that.
				if event.Phase == "tool_completed" && previousOperation != nil && !registry.IsReadOnly(previousOperation.Name) && len(event.ToolTraces) > 0 {
					last := event.ToolTraces[len(event.ToolTraces)-1]
					if last.BlockID == previousOperation.ID && last.Error {
						cp.PendingTool = previousOperation
						cp.Phase = "reconciliation"
						return errors.Join(ErrGraphActionUncertain, persist(persistCtx))
					}
				}
				return persist(persistCtx)
			},
		}, string(input))
		if out != nil {
			cp.Output = stripEvidenceMarkers(stripPlanMarkers(scratchpadRE.ReplaceAllString(out.Text, "")))
			cp.Traces = append(append([]agent.ToolTrace(nil), previousTraces...), out.ToolTraces...)
		}
		if runErr != nil {
			return result(), false, runErr
		}
		if strings.TrimSpace(cp.Output) == "" {
			return result(), false, fmt.Errorf("graph step produced no candidate")
		}
		if task.ExecutorVersion == 2 && (step.Kind == "read" || (step.Kind == "finalize" && len(step.Tools) == 0)) {
			rendered, calculationErr := renderGraphReport(ctx, task, deps, cp.Output)
			if calculationErr != nil {
				cp.CandidateReady = true
				cp.Phase = "rejected"
				cp.ReviewReason = "Correct the report marker: " + calculationErr.Error()
				return result(), false, persist(ctx)
			}
			cp.Output = rendered
		}
		cp.CandidateReady = true
		cp.Phase = "verification"
		cp.PendingTool = nil
		if err := persist(ctx); err != nil {
			return result(), false, err
		}
	}
	if step.Kind == "finalize" && len(step.Tools) == 0 && deps.FinalAcceptanceOwned && task.AcceptanceCriteria != nil && strings.TrimSpace(*task.AcceptanceCriteria) != "" {
		// Assembly is complete, but task success still requires the scheduler's
		// full acceptance and grounding gates. No downstream step consumes this
		// candidate because the graph finalizer must be the sole terminal node.
		cp.Phase = "assembled"
		if err := persist(ctx); err != nil {
			return result(), false, err
		}
		return result(), true, nil
	}
	if step.Kind == "action" {
		receipt := false
		for _, r := range cp.Receipts {
			if !r.IsError && strings.TrimSpace(r.Output) != "" {
				receipt = true
			}
		}
		if !receipt {
			// Not repeated (no duplicate effects) and not a reason to stop the
			// task: the deliverable states that the operation was not performed.
			cp.Output = unverifiedStepResult(cp.Output, "no successful operation receipt; the requested operation was not performed")
			cp.Phase = "verified"
			if err := persist(ctx); err != nil {
				return result(), false, err
			}
			return result(), true, nil
		}
	}
	prompt, err := deps.Prompts.Get(ctx, "background-graph-review")
	if err != nil || strings.TrimSpace(prompt) == "" {
		return result(), false, fmt.Errorf("%w: review prompt unavailable", ErrGraphVerificationUnavailable)
	}
	reviewInput, err := json.Marshal(map[string]any{"current_datetime": b.graphCurrentDatetime(ctx, deps),
		"step_id": step.ID, "assigned_to_other_steps": otherGoals,
		"task": task.Title, "task_description": task.Description, "task_acceptance": task.AcceptanceCriteria,
		"kind": step.Kind, "goal": step.Goal, "acceptance": step.Acceptance, "confirmed_inputs": inputs,
		"candidate": cp.Output, "receipts": cp.Receipts,
	})
	if err != nil {
		return result(), false, err
	}
	cp.ReviewAttempts++
	if err := persist(ctx); err != nil {
		return result(), false, err
	}
	reviewCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	review, err := deps.LLM.Complete(reviewCtx, core.CompletionRequest{Model: model.ForRouter(), System: prompt, Messages: []core.Message{{Role: "user", Content: core.NormalizeContent(string(reviewInput))}}, MaxTokens: 4096, Temperature: 0.01, Effort: model.Effort, ThinkingMode: model.ThinkingMode})
	if err != nil || review == nil {
		return result(), false, errors.Join(ErrGraphVerificationUnavailable, err)
	}
	if review.StopReason == "max_tokens" {
		return result(), false, fmt.Errorf("%w: review output token limit reached", ErrGraphVerificationUnavailable)
	}
	var text strings.Builder
	for _, block := range review.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	var verdict struct {
		Accepted *bool  `json:"accepted"`
		Reason   string `json:"reason"`
	}
	decoder := json.NewDecoder(strings.NewReader(text.String()))
	// Only accepted/reason have meaning. Extra metadata cannot change the
	// verdict or request actions; a missing/invalid accepted field still fails.
	if err := decoder.Decode(&verdict); err != nil || verdict.Accepted == nil {
		return result(), false, ErrGraphVerificationUnavailable
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return result(), false, ErrGraphVerificationUnavailable
	}
	cp.ReviewReason = verdict.Reason
	accepted := *verdict.Accepted
	lastRead := step.Kind == "read" && step.MaxAttempts > 0 && step.Attempts >= step.MaxAttempts
	if !accepted && (lastRead || step.Kind == "action") {
		// Blocking stops the whole task short of its deliverable. Hand the
		// candidate on with its unmet acceptance explicit; an action is never
		// repeated. Task acceptance still decides completed vs partial.
		cp.Output = unverifiedStepResult(cp.Output, verdict.Reason)
		accepted = true
	}
	if accepted {
		cp.Phase = "verified"
	} else {
		cp.Phase = "rejected"
	}
	if err := persist(ctx); err != nil {
		return result(), false, err
	}
	return result(), accepted, nil
}

func unverifiedStepResult(candidate, gaps string) string {
	return strings.TrimSpace(candidate) + "\n\n[unverified_step_result]\nThis step did not pass its acceptance review after its last attempt. Outstanding gaps: " + strings.TrimSpace(gaps) + "\nUse only facts its sources support and state these gaps explicitly in the deliverable.\n[/unverified_step_result]"
}

func graphToolTurnLimit(deps core.AgentDeps) int {
	limit := deps.Config.Gateway.MaxTurns
	if limit <= 0 {
		return 15 // Match GatewayConfig's default when deps were not defaulted.
	}
	// The configured tool-turn limit and research deadline bound the work.
	// A second, hidden eight-turn cap forces incomplete candidates through
	// synthesis/review/repair even while the task has research time remaining.
	return limit
}
