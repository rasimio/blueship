package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rasimio/blueship/agent"
	"github.com/rasimio/blueship/core"
)

// PlanStep represents one step in a goal execution plan.
type PlanStep struct {
	Action      string          `json:"action"`                 // "tool", "wait", "decide", "milestone", "done"
	Tool        string          `json:"tool,omitempty"`         // tool name for "tool" action
	Input       json.RawMessage `json:"input,omitempty"`        // tool input (supports $variable substitution)
	Question    string          `json:"question,omitempty"`     // for "decide" action
	ContextTool string          `json:"context_tool,omitempty"` // tool to call before decision (e.g. code_task_status)
	Message     string          `json:"message,omitempty"`      // for "milestone" action
}

// goalPlanProgress stores plan execution state between iterations.
type goalPlanProgress struct {
	SessionID   string          `json:"session_id"`
	PeerTaskID  string          `json:"peer_task_id,omitempty"`
	Plan        []PlanStep      `json:"plan,omitempty"`
	CurrentStep int             `json:"current_step"`
	LastResult  json.RawMessage `json:"last_result,omitempty"` // result from last tool call
	Phase       string          `json:"phase"`
	Summary     string          `json:"summary"`
}

// runPlanExecutor handles goal tasks using the plan-then-execute pattern.
// First iteration: LLM creates a structured plan.
// Subsequent iterations: handler executes steps mechanically, consulting LLM only at decision points.
func (b *Background) runPlanExecutor(ctx context.Context, task core.AgentTask, deps core.AgentDeps) (core.IterationResult, error) {
	// Parse progress.
	var progress goalPlanProgress
	if len(task.Progress) > 0 && string(task.Progress) != "{}" {
		json.Unmarshal(task.Progress, &progress)
	}

	// Resolve model.
	modelRole := "cortex"
	if task.Config != nil {
		var roleCfg struct {
			ModelRole string `json:"model_role"`
		}
		if json.Unmarshal(task.Config, &roleCfg) == nil && roleCfg.ModelRole != "" {
			modelRole = roleCfg.ModelRole
		}
	}
	routerModel := deps.Config.Models.Primary.ForRouter()
	displayModel := deps.Config.Models.Primary.Name
	if deps.ModelStore != nil {
		if m := deps.ModelStore.ForRouter(modelRole); m != "" {
			routerModel = m
		}
		if ref := deps.ModelStore.Get(modelRole); ref.Name != "" {
			displayModel = ref.Name
		}
	}

	// Get or create session.
	sessID := progress.SessionID
	if sessID == "" {
		var err error
		sessID, err = deps.Store.CreateSessionWithSource(ctx, task.UserID.String(), displayModel, "agent_task", task.ID.String())
		if err != nil {
			return core.IterationResult{}, fmt.Errorf("create session: %w", err)
		}
		progress.SessionID = sessID
	}

	// No plan yet → PLANNING PHASE.
	if len(progress.Plan) == 0 {
		return b.planPhase(ctx, task, deps, &progress, sessID, routerModel)
	}

	// Have plan → EXECUTION PHASE.
	return b.executeStep(ctx, task, deps, &progress, sessID, routerModel)
}

// planPhase asks the LLM to create a structured plan for the goal.
func (b *Background) planPhase(ctx context.Context, task core.AgentTask, deps core.AgentDeps, progress *goalPlanProgress, sessID, model string) (core.IterationResult, error) {
	desc := ""
	if task.Description != nil {
		desc = *task.Description
	}

	// Load system prompt.
	var parts []string
	promptKeys := append(deps.Config.SystemPromptKeys, "goal-orchestrator")
	for _, key := range promptKeys {
		p, err := deps.Prompts.Get(ctx, key)
		if err != nil {
			return core.IterationResult{}, fmt.Errorf("load prompt %q: %w", key, err)
		}
		parts = append(parts, p)
	}
	systemPrompt := strings.Join(parts, "\n\n")

	now := time.Now().In(b.tz)
	systemPrompt = fmt.Sprintf("[current_datetime: %s]\n\n%s", now.Format("2006-01-02 15:04 MST (Monday)"), systemPrompt)

	// Run reflex pipeline for context.
	reflex := runReflexPipeline(ctx, deps, b.tz, desc)

	// Build planning prompt.
	planPrompt := fmt.Sprintf(`[GOAL: %s]
%s

Create a step-by-step execution plan as a JSON array. Each step must have:
- "action": one of "tool", "wait", "decide", "milestone", "done"
- "tool": tool name (for "tool" action)
- "input": JSON object with tool parameters (for "tool" action). Use $peer_task_id for the current task ID.
- "question": what to evaluate (for "decide" action)
- "context_tool": tool to call before decision to get data, e.g. "code_task_status" (for "decide" action)
- "message": notification text (for "milestone" action)

Available tools: %s

Rules:
- "wait" pauses until external callback (use after creating async tasks)
- "decide" asks the LLM to evaluate and choose APPROVE or REVISE
- "milestone" notifies the user and pauses for approval
- End the plan with {"action": "done"}

Output ONLY the JSON array. No explanations.`, task.Title, desc, buildToolsList(deps))

	// Inject context.
	var injectedCtx string
	if reflex.InjectedCtx != "" {
		injectedCtx = reflex.InjectedCtx
	}
	if reflex.Guidance != "" {
		if injectedCtx != "" {
			injectedCtx += "\n\n" + reflex.Guidance
		} else {
			injectedCtx = reflex.Guidance
		}
	}

	loop := agent.NewLoop(deps.LLM, deps.Store, deps.Registry, deps.RoleTools, deps.Config, deps.Logger)
	reply, err := loop.Run(ctx, agent.RunConfig{
		SessionID:       sessID,
		SystemPrompt:    systemPrompt,
		InjectedContext: injectedCtx,
		Model:           model,
		MaxTokens:       deps.Config.Limits.MaxOutputTokens,
		MaxTurns:        1, // single turn — just output the plan
		Role:            "cortex",
	}, planPrompt)
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("planning: %w", err)
	}

	// Parse plan from LLM output.
	plan, parseErr := parsePlan(reply)
	if parseErr != nil {
		deps.Logger.Warn("plan-executor: failed to parse plan, retrying next iteration",
			"error", parseErr, "reply_len", len(reply))
		progress.Phase = "plan_parse_error"
		progress.Summary = fmt.Sprintf("Plan parse error: %v. Reply: %s", parseErr, truncate(reply, 300))
		progressJSON, _ := json.Marshal(progress)
		return core.IterationResult{Progress: progressJSON}, nil
	}

	progress.Plan = plan
	progress.CurrentStep = 0
	progress.Phase = "executing"
	progress.Summary = fmt.Sprintf("Plan created with %d steps", len(plan))
	progressJSON, _ := json.Marshal(progress)

	deps.Logger.Info("plan-executor: plan created", "steps", len(plan))

	// Don't pause — immediately start execution on next tick.
	return core.IterationResult{Progress: progressJSON}, nil
}

// executeStep runs one step from the plan.
func (b *Background) executeStep(ctx context.Context, task core.AgentTask, deps core.AgentDeps, progress *goalPlanProgress, sessID, model string) (core.IterationResult, error) {
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
		return b.execToolStep(ctx, deps, progress, step)

	case "wait":
		progress.Phase = "waiting"
		progress.Summary = fmt.Sprintf("Waiting for callback (step %d/%d)", progress.CurrentStep+1, len(progress.Plan))
		progress.CurrentStep++
		progressJSON, _ := json.Marshal(progress)
		return core.IterationResult{Pause: true, Progress: progressJSON}, nil

	case "decide":
		return b.execDecideStep(ctx, task, deps, progress, step, sessID, model)

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
		summary := fmt.Sprintf("[DONE] %s", progress.Summary)
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
func (b *Background) execToolStep(ctx context.Context, deps core.AgentDeps, progress *goalPlanProgress, step PlanStep) (core.IterationResult, error) {
	// Substitute variables in input.
	input := substituteVars(step.Input, progress)

	result, isError := deps.Registry.Execute(ctx, step.Tool, input)
	if isError {
		deps.Logger.Warn("plan-executor: tool error", "tool", step.Tool, "error", result)
		progress.Phase = "tool_error"
		progress.Summary = fmt.Sprintf("Tool %s failed: %s", step.Tool, truncate(result, 300))
		progressJSON, _ := json.Marshal(progress)
		// Don't advance — let next iteration handle (replan or retry).
		return core.IterationResult{Progress: progressJSON}, nil
	}

	deps.Logger.Info("plan-executor: tool success", "tool", step.Tool, "result_len", len(result))

	// Extract peer_task_id from result if present.
	var resultMap map[string]any
	if json.Unmarshal([]byte(result), &resultMap) == nil {
		if tid, ok := resultMap["task_id"].(string); ok && tid != "" {
			progress.PeerTaskID = tid
		}
		// Store full result for variable substitution.
		progress.LastResult = json.RawMessage(result)
	}

	progress.CurrentStep++
	progress.Phase = fmt.Sprintf("step_%d", progress.CurrentStep)
	progress.Summary = fmt.Sprintf("Completed: %s (step %d/%d)", step.Tool, progress.CurrentStep, len(progress.Plan))
	progressJSON, _ := json.Marshal(progress)

	// Check if next step is "wait" — if so, execute it immediately (no extra iteration).
	if progress.CurrentStep < len(progress.Plan) && progress.Plan[progress.CurrentStep].Action == "wait" {
		progress.Phase = "waiting"
		progress.CurrentStep++
		progressJSON, _ = json.Marshal(progress)
		return core.IterationResult{Pause: true, Progress: progressJSON}, nil
	}

	return core.IterationResult{Progress: progressJSON}, nil
}

// execDecideStep asks the LLM to make a binary decision.
func (b *Background) execDecideStep(ctx context.Context, task core.AgentTask, deps core.AgentDeps, progress *goalPlanProgress, step PlanStep, sessID, model string) (core.IterationResult, error) {
	// Fetch context data if context_tool specified.
	var contextData string
	if step.ContextTool != "" {
		input := substituteVars(json.RawMessage(fmt.Sprintf(`{"task_id":"%s"}`, progress.PeerTaskID)), progress)
		result, isError := deps.Registry.Execute(ctx, step.ContextTool, input)
		if !isError {
			contextData = result
		}
	}

	// Build constrained decision prompt.
	question := step.Question
	if question == "" {
		question = "Should we proceed?"
	}

	decisionPrompt := fmt.Sprintf(`You are reviewing step %d of a goal plan for: %s

Data:
%s

Question: %s

Answer ONLY one of:
- APPROVE (to continue to next step)
- REVISE: <specific feedback for what to change>

No explanations. Just APPROVE or REVISE: feedback.`,
		progress.CurrentStep+1, task.Title,
		truncate(contextData, 3000),
		question)

	// Load system prompt.
	systemPrompt := "You are a decision-making assistant. Output ONLY 'APPROVE' or 'REVISE: <feedback>'. Nothing else."

	loop := agent.NewLoop(deps.LLM, deps.Store, deps.Registry, deps.RoleTools, deps.Config, deps.Logger)
	reply, err := loop.Run(ctx, agent.RunConfig{
		SessionID:    sessID,
		SystemPrompt: systemPrompt,
		Model:        model,
		MaxTokens:    256,
		MaxTurns:     1,
		Role:         "cortex",
	}, decisionPrompt)
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("decide: %w", err)
	}

	reply = strings.TrimSpace(reply)
	deps.Logger.Info("plan-executor: decision", "reply", truncate(reply, 100))

	if strings.HasPrefix(strings.ToUpper(reply), "APPROVE") {
		progress.CurrentStep++
		progress.Phase = fmt.Sprintf("step_%d", progress.CurrentStep)
		progress.Summary = fmt.Sprintf("Approved step %d, continuing", progress.CurrentStep)
		progressJSON, _ := json.Marshal(progress)
		return core.IterationResult{Progress: progressJSON}, nil
	}

	// REVISE — extract feedback, call code_task_revise if peer task exists.
	feedback := reply
	if idx := strings.Index(reply, ":"); idx >= 0 {
		feedback = strings.TrimSpace(reply[idx+1:])
	}

	if progress.PeerTaskID != "" {
		input := json.RawMessage(fmt.Sprintf(`{"task_id":"%s","feedback":"%s"}`,
			progress.PeerTaskID, strings.ReplaceAll(feedback, `"`, `\"`)))
		_, _ = deps.Registry.Execute(ctx, "code_task_revise", input)
	}

	// After revise, go back to wait for the peer task to be redone.
	progress.Phase = "waiting"
	progress.Summary = fmt.Sprintf("Revised: %s", truncate(feedback, 200))
	// Don't advance CurrentStep — stay at decide, will re-evaluate after callback.
	progressJSON, _ := json.Marshal(progress)
	return core.IterationResult{Pause: true, Progress: progressJSON}, nil
}

// substituteVars replaces $peer_task_id and $result.X in input JSON.
func substituteVars(input json.RawMessage, progress *goalPlanProgress) json.RawMessage {
	if len(input) == 0 {
		return input
	}
	s := string(input)
	s = strings.ReplaceAll(s, "$peer_task_id", progress.PeerTaskID)

	// Substitute $result.field references.
	if len(progress.LastResult) > 0 {
		var lastResult map[string]any
		if json.Unmarshal(progress.LastResult, &lastResult) == nil {
			for k, v := range lastResult {
				placeholder := fmt.Sprintf("$result.%s", k)
				if str, ok := v.(string); ok {
					s = strings.ReplaceAll(s, placeholder, str)
				}
			}
		}
	}

	return json.RawMessage(s)
}

// parsePlan extracts a JSON array of PlanStep from LLM output.
func parsePlan(reply string) ([]PlanStep, error) {
	reply = strings.TrimSpace(reply)

	// Strip markdown code fences.
	if strings.HasPrefix(reply, "```") {
		lines := strings.Split(reply, "\n")
		if len(lines) > 2 {
			reply = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	// Try direct parse.
	var steps []PlanStep
	if err := json.Unmarshal([]byte(reply), &steps); err == nil && len(steps) > 0 {
		return steps, nil
	}

	// Try to find JSON array in the reply.
	start := strings.Index(reply, "[")
	end := strings.LastIndex(reply, "]")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(reply[start:end+1]), &steps); err == nil && len(steps) > 0 {
			return steps, nil
		}
	}

	return nil, fmt.Errorf("no valid JSON plan found in reply (%d chars)", len(reply))
}
