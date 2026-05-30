package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

func (e *StructuredGoalExecutor) executeStep(ctx context.Context, task core.AgentTask, deps core.AgentDeps, progress *goalPlanProgress, sessID, model, modelRole string) (core.IterationResult, error) {
	if progress.CurrentStep >= len(progress.Plan) {
		// Plan exhausted without explicit "done" step.
		return core.IterationResult{Done: true, Output: progress.Summary, Notify: progress.Summary}, nil
	}

	step := progress.Plan[progress.CurrentStep]
	deps.Logger.Info("plan-executor: executing step",
		"step", progress.CurrentStep,
		"action", step.Action,
		"tool", step.Tool,
	)

	switch step.Action {
	case "tool":
		return e.execToolStep(ctx, deps, progress, step)

	case "wait":
		// Only pause if we're actually tracking a peer task (async operation).
		// Skip wait if no peer_task_id — means previous tool was sync, no callback expected.
		if progress.PeerTaskID == "" {
			deps.Logger.Info("plan-executor: skipping wait (no peer task)", "step", progress.CurrentStep)
			progress.CurrentStep++
			progressJSON, _ := json.Marshal(progress)
			return core.IterationResult{Progress: progressJSON}, nil
		}
		progress.Phase = "waiting"
		progress.Summary = fmt.Sprintf("Waiting for callback (step %d/%d)", progress.CurrentStep+1, len(progress.Plan))
		progress.CurrentStep++
		progressJSON, _ := json.Marshal(progress)
		return core.IterationResult{Pause: true, Progress: progressJSON}, nil

	case "decide":
		return e.execDecideStep(ctx, task, deps, progress, step, sessID, model, modelRole)

	case "milestone":
		msg := step.Message
		if msg == "" {
			msg = fmt.Sprintf("%s — milestone reached (step %d/%d)", task.Title, progress.CurrentStep+1, len(progress.Plan))
		}
		progress.CurrentStep++
		progress.Phase = "milestone"
		progress.Summary = msg
		progressJSON, _ := json.Marshal(progress)
		return core.IterationResult{Pause: true, Progress: progressJSON, Notify: msg}, nil

	case "done":
		// Build structured completion report.
		var report strings.Builder
		report.WriteString(fmt.Sprintf("[DONE] %s\n\n", task.Title))
		report.WriteString(fmt.Sprintf("Steps completed: %d/%d\n", progress.CurrentStep, len(progress.Plan)))
		report.WriteString(fmt.Sprintf("Iterations used: %d/%d\n", task.Iteration+1, task.MaxIterations))
		if progress.RepoPath != "" {
			report.WriteString(fmt.Sprintf("Repository: %s\n", progress.RepoPath))
		}
		if progress.PeerTaskID != "" {
			report.WriteString(fmt.Sprintf("Task ID: %s\n", progress.PeerTaskID))
		}
		report.WriteString(fmt.Sprintf("\nLast step: %s", progress.Summary))

		summary := report.String()
		deps.Store.ArchiveSession(context.Background(), sessID)
		return core.IterationResult{Done: true, Output: summary, Notify: summary}, nil

	default:
		deps.Logger.Warn("plan-executor: unknown action, skipping", "action", step.Action)
		progress.CurrentStep++
		progressJSON, _ := json.Marshal(progress)
		return core.IterationResult{Progress: progressJSON}, nil
	}
}

// execToolStep calls a tool directly and advances the plan.
func (e *StructuredGoalExecutor) execToolStep(ctx context.Context, deps core.AgentDeps, progress *goalPlanProgress, step PlanStep) (core.IterationResult, error) {
	// Substitute variables in input.
	input := substituteVars(step.Input, progress)

	deps.Logger.Info("plan-executor: tool input",
		"tool", step.Tool,
		"input", truncate(string(input), 150),
		"last_result_len", len(progress.LastResult),
		"peer_task_id", progress.PeerTaskID,
	)

	result, isError := deps.Registry.Execute(ctx, step.Tool, input)
	if isError {
		// "already exists" is not a real error — resource is there, move on.
		// We don't try to recover the missing fields (e.g. repo_path) here;
		// that's a plan-authoring concern. If the plan needs a specific
		// piece of data after an "already_exists" success, it should
		// include a follow-up step (e.g. a listing/read tool) rather than
		// relying on the executor to know how.
		if strings.Contains(result, "already exists") {
			deps.Logger.Info("plan-executor: resource already exists, treating as success", "tool", step.Tool)
			isError = false
		} else {
			progress.RetryCount++
			deps.Logger.Warn("plan-executor: tool error", "tool", step.Tool, "retry", progress.RetryCount, "error", result)
			if progress.RetryCount >= maxStepRetries {
				summary := fmt.Sprintf("Goal failed: tool %s errored %d times. Last error: %s",
					step.Tool, progress.RetryCount, truncate(result, 300))
				deps.Logger.Warn("plan-executor: fail-fast (retry cap)", "tool", step.Tool, "retries", progress.RetryCount)
				return core.IterationResult{Done: true, Output: summary, Notify: summary}, nil
			}
			progress.Phase = "tool_error"
			progress.Summary = fmt.Sprintf("Tool %s failed (attempt %d): %s", step.Tool, progress.RetryCount, truncate(result, 200))
			progressJSON, _ := json.Marshal(progress)
			return core.IterationResult{Progress: progressJSON}, nil
		}
	}

	progress.RetryCount = 0 // reset on success
	deps.Logger.Info("plan-executor: tool success", "tool", step.Tool, "result_len", len(result), "result_preview", truncate(result, 100))

	// Extract a small set of GENERIC well-known fields from the result.
	// These are conventions any tool can opt into, not domain-specific
	// assumptions: a task_id string becomes the peer_task_id for
	// subsequent $peer_task_id substitution; a repo_path (or path, as
	// some repo-scoped tools return it) becomes $result.repo_path /
	// $result.path. Everything else sits in progress.LastResult and the
	// plan's variable-substitution layer resolves it on demand.
	var resultMap map[string]any
	if json.Unmarshal([]byte(result), &resultMap) == nil {
		if tid, ok := resultMap["task_id"].(string); ok && tid != "" {
			progress.PeerTaskID = tid
		}
		if rp, ok := resultMap["repo_path"].(string); ok && rp != "" {
			progress.RepoPath = rp
		}
		if rp, ok := resultMap["path"].(string); ok && rp != "" && progress.RepoPath == "" {
			progress.RepoPath = rp
		}
		progress.LastResult = json.RawMessage(result)
	}

	progress.CurrentStep++
	progress.Phase = fmt.Sprintf("step_%d", progress.CurrentStep)
	progress.Summary = fmt.Sprintf("Completed: %s (step %d/%d)", step.Tool, progress.CurrentStep, len(progress.Plan))
	progressJSON, _ := json.Marshal(progress)

	// Check if next step is "wait" — if so, execute it immediately (no extra iteration).
	// But only pause if we have a peer_task_id (an async task to wait for).
	if progress.CurrentStep < len(progress.Plan) && progress.Plan[progress.CurrentStep].Action == "wait" {
		if progress.PeerTaskID != "" {
			// Record this tool step index as the "last async" so a future
			// REVISE can rewind here without scanning for specific tool
			// names. CurrentStep currently points to the wait step; the
			// tool step that produced the peer_task_id is just before it.
			progress.LastAsyncStepIdx = progress.CurrentStep - 1
			progress.Phase = "waiting"
			progress.CurrentStep++
			progressJSON, _ = json.Marshal(progress)
			return core.IterationResult{Pause: true, Progress: progressJSON}, nil
		}
		// No peer task — skip the wait.
		progress.CurrentStep++
		progressJSON, _ = json.Marshal(progress)
	}

	return core.IterationResult{Progress: progressJSON}, nil
}

// execDecideStep asks the LLM to make a binary decision.
func (e *StructuredGoalExecutor) execDecideStep(ctx context.Context, task core.AgentTask, deps core.AgentDeps, progress *goalPlanProgress, step PlanStep, sessID, model, modelRole string) (core.IterationResult, error) {
	// Fetch context data if context_tool specified.
	var contextData string
	if step.ContextTool != "" {
		input := substituteVars(json.RawMessage(fmt.Sprintf(`{"task_id":"%s"}`, progress.PeerTaskID)), progress)
		result, isError := deps.Registry.Execute(ctx, step.ContextTool, input)
		if !isError {
			contextData = result
		}
	}

	// Parse the context data as a flat JSON object. This is generic —
	// we don't assume any particular schema. Anything we do with fields
	// is gated by declarative knobs on the plan step (Precondition).
	var contextObj map[string]any
	var contextParsed bool
	if contextData != "" {
		contextParsed = json.Unmarshal([]byte(contextData), &contextObj) == nil
	}

	// Declarative auto-REVISE: if the step has a Precondition and any
	// clause fails, revise without consulting the LLM. No hardcoded
	// tool names or field schemas — the plan author decides what
	// "ready" looks like for the step they're guarding.
	if step.Precondition != nil && contextParsed {
		if failMsg := evalPrecondition(step.Precondition, contextObj); failMsg != "" {
			auto := fmt.Sprintf("REVISE: %s", failMsg)
			deps.Logger.Info("plan-executor: auto-REVISE (precondition failed)", "reason", failMsg)
			return e.handleRevise(ctx, deps, progress, step, auto)
		}
	}

	// Build constrained decision prompt.
	question := step.Question
	if question == "" {
		question = "Should we proceed?"
	}

	// Load decision prompts from DB.
	decideSystem, _ := deps.Prompts.Get(ctx, "goal-decide-system")
	decideUser, _ := deps.Prompts.Get(ctx, "goal-decide-user")

	// Generic metadata header: pick scalar top-level fields from the
	// context JSON and print them compactly. Large strings are summarised
	// as "<field> (string, N chars)" so the reviewer sees they exist
	// without the value eating the truncation budget.
	contextBlock := truncate(contextData, 3000)
	if contextParsed {
		contextBlock = formatMetadata(contextObj) + "\n\nRaw context JSON (truncated):\n" + contextBlock
	}

	decisionPrompt := fmt.Sprintf(decideUser,
		progress.CurrentStep+1, task.Title,
		contextBlock,
		question, progress.RetryCount)

	systemPrompt := decideSystem

	loop := agent.NewLoop(deps.LLM, deps.Store, deps.Registry, deps.RoleTools, deps.Config, deps.Logger)
	reply, err := loop.Run(ctx, agent.RunConfig{
		SessionID:    sessID,
		SystemPrompt: systemPrompt,
		Model:        model,
		MaxTokens:    256,
		MaxTurns:     1,
		Role:         modelRole,
	}, decisionPrompt)
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("decide: %w", err)
	}

	reply = strings.TrimSpace(reply)
	deps.Logger.Info("plan-executor: decision", "reply", truncate(reply, 100))

	if strings.HasPrefix(strings.ToUpper(reply), "APPROVE") {
		progress.CurrentStep++
		progress.RetryCount = 0
		progress.ReviseCount = 0
		progress.Phase = fmt.Sprintf("step_%d", progress.CurrentStep)
		progress.Summary = fmt.Sprintf("Approved step %d, continuing", progress.CurrentStep)
		progressJSON, _ := json.Marshal(progress)
		return core.IterationResult{Progress: progressJSON}, nil
	}

	return e.handleRevise(ctx, deps, progress, step, reply)
}

// handleRevise processes a REVISE decision:
//   - Increments ReviseCount (not RetryCount — tool success wipes
//     RetryCount, which would mask a decide loop).
//   - Caps at maxStepRetries and fails the goal if exceeded.
//   - If the decide step declared an OnRevise tool call, invokes it
//     with `{feedback}` and `$*` placeholders substituted. Typical use:
//     call a peer's revise endpoint so the peer regenerates before we
//     re-run the async step.
//   - Rewinds CurrentStep to progress.LastAsyncStepIdx (the most recent
//     tool step that paused on a peer callback) so the next iteration
//     re-drives the async path. If no async step was ever reached,
//     stays in place.
func (e *StructuredGoalExecutor) handleRevise(ctx context.Context, deps core.AgentDeps, progress *goalPlanProgress, step PlanStep, reply string) (core.IterationResult, error) {
	progress.ReviseCount++

	feedback := reply
	if idx := strings.Index(reply, ":"); idx >= 0 {
		feedback = strings.TrimSpace(reply[idx+1:])
	}

	// Fix C: cap REVISE attempts.
	if progress.ReviseCount >= maxStepRetries {
		summary := fmt.Sprintf("Goal failed: %d revise attempts exhausted. Last feedback: %s",
			progress.ReviseCount, truncate(feedback, 300))
		deps.Logger.Warn("plan-executor: fail-fast (revise cap)", "revises", progress.ReviseCount)
		return core.IterationResult{Done: true, Output: summary, Notify: summary}, nil
	}

	// Optional: fire the plan's declared revise-callback tool so the
	// peer knows to regenerate. The plan author supplies the tool name
	// and input template declaratively, or omits it for purely local
	// re-plans.
	if step.OnRevise != nil && step.OnRevise.Tool != "" {
		var input json.RawMessage
		if len(step.OnRevise.Input) > 0 {
			// {feedback} substitution + standard $var substitution.
			raw := string(step.OnRevise.Input)
			raw = strings.ReplaceAll(raw, "{feedback}", strings.ReplaceAll(feedback, `"`, `\"`))
			input = substituteVars(json.RawMessage(raw), progress)
		} else {
			input = substituteVars(json.RawMessage(fmt.Sprintf(
				`{"task_id":"%s","feedback":"%s"}`,
				progress.PeerTaskID, strings.ReplaceAll(feedback, `"`, `\"`))), progress)
		}
		_, _ = deps.Registry.Execute(ctx, step.OnRevise.Tool, input)
	}

	// Rewind CurrentStep to the most recent async (paused) step so the
	// next iteration re-drives execution on the regenerated peer state.
	if progress.LastAsyncStepIdx >= 0 && progress.LastAsyncStepIdx < progress.CurrentStep {
		deps.Logger.Info("plan-executor: REVISE rewind",
			"from_step", progress.CurrentStep, "to_step", progress.LastAsyncStepIdx,
			"revise_count", progress.ReviseCount)
		progress.CurrentStep = progress.LastAsyncStepIdx
	}

	progress.Phase = "waiting_for_revise"
	progress.Summary = fmt.Sprintf("Revised (attempt %d): %s", progress.ReviseCount, truncate(feedback, 200))
	progressJSON, _ := json.Marshal(progress)
	return core.IterationResult{Pause: true, Progress: progressJSON}, nil
}

// evalPrecondition returns an empty string if the step's declarative
// precondition passes against the given context object, or a human-readable
// failure message otherwise. No schema assumptions — works against any
// JSON-deserialised object.
