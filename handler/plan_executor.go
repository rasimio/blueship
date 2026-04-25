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
//
// The plan author (the planning LLM + plan template) chooses which fields
// are used per step. Only `Action` is always required. The executor does
// not inspect tool names or make domain assumptions — everything that
// would require such knowledge (precondition guard for decide, revise
// callback, etc.) is expressed declaratively in the step itself.
type PlanStep struct {
	Action      string          `json:"action"`                 // "tool", "wait", "decide", "milestone", "done"
	Tool        string          `json:"tool,omitempty"`         // tool name for "tool" action
	Input       json.RawMessage `json:"input,omitempty"`        // tool input (supports $variable substitution)
	Question    string          `json:"question,omitempty"`     // for "decide" action
	ContextTool string          `json:"context_tool,omitempty"` // tool to call before decide to build reviewer context
	Message     string          `json:"message,omitempty"`      // for "milestone" action

	// Precondition (optional, decide steps) is a declarative guard that
	// auto-REVISEs the decide BEFORE consulting the LLM when expected
	// artifacts are not present in the context_tool result. Cheap safety
	// net against reviewers approving half-finished work on a truncated
	// context view.
	Precondition *PlanPrecondition `json:"precondition,omitempty"`

	// OnRevise (optional, decide steps) is an invocation that the
	// executor fires whenever this decide returns REVISE. Typical use:
	// call a peer's `*_revise` tool with the reviewer feedback so the
	// peer regenerates before we re-enter the execute step. If absent,
	// REVISE just rewinds CurrentStep and pauses without external side
	// effects.
	OnRevise *PlanToolCall `json:"on_revise,omitempty"`
}

// PlanPrecondition is a declarative guard for decide steps. It operates
// on the top-level JSON returned by the decide step's context_tool.
//
//   StatusIn   — the result's `status` field must equal one of these.
//   AnyPresent — at least one of these result keys must be non-empty
//                (any scalar != "" / != 0 / != null / != false).
//
// If neither list is set the precondition is a no-op. If either fails,
// the executor auto-REVISEs with a structured message naming the failing
// condition (no LLM call, no budget burned).
type PlanPrecondition struct {
	StatusIn   []string `json:"status_in,omitempty"`
	AnyPresent []string `json:"any_present,omitempty"`
}

// PlanToolCall is an embeddable invocation spec: which tool to call and
// what input template to use. `{feedback}` in Input is replaced with the
// reviewer's REVISE feedback at call time; other `$*` placeholders use
// normal plan-variable substitution.
type PlanToolCall struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input,omitempty"`
}

// goalPlanProgress stores plan execution state between iterations.
type goalPlanProgress struct {
	SessionID   string     `json:"session_id"`
	PeerTaskID  string     `json:"peer_task_id,omitempty"`
	RepoPath    string     `json:"repo_path,omitempty"`
	Plan        []PlanStep `json:"plan,omitempty"`
	CurrentStep int        `json:"current_step"`
	// RetryCount is the consecutive tool-error count on the current step.
	// Reset to 0 when any tool call succeeds.
	RetryCount int `json:"retry_count,omitempty"`
	// ReviseCount is the number of REVISE decisions issued on this step. It
	// accumulates across the execute→decide→REVISE→rewind→execute cycle,
	// because a successful re-execution does NOT mean the reviewer will
	// approve the next time. Persists separately from RetryCount so that
	// tool retries (which reset on success) don't mask a decide loop.
	ReviseCount int `json:"revise_count,omitempty"`
	// LastAsyncStepIdx is the plan index of the most recent tool step that
	// paused waiting for a peer callback. REVISE rewinds to this step so
	// the executor re-drives the async path on the regenerated plan.
	// -1 = none seen yet. NO omitempty: the value 0 is meaningful (first
	// step in plan) and would otherwise be stripped by the JSON marshaler,
	// and on subsequent unmarshal the initializer default (-1) would take
	// over, corrupting the rewind target.
	LastAsyncStepIdx int             `json:"last_async_step_idx"`
	LastResult       json.RawMessage `json:"last_result,omitempty"`
	Phase            string          `json:"phase"`
	Summary          string          `json:"summary"`
}

// maxStepRetries caps how many times we retry any single step (tool error or REVISE)
// before failing the goal. Prevents infinite loops on fatal states like a peer task
// that cannot produce a branch or an LLM that keeps rejecting a correct result.
const maxStepRetries = 3

// StructuredGoalExecutor implements core.GoalHandler for goals whose
// strategy is "structured" — the LLM generates a JSON plan up front
// and the executor interprets steps mechanically, consulting the LLM
// again only at explicit decide gates.
//
// Generic, agent-agnostic. It does NOT know about any specific tool
// names. Behaviours that feel domain-specific (auto-REVISE when a
// peer's result doesn't meet expectations, invoking a peer's revise
// endpoint on reject, rewinding to the last async step) are all
// expressed declaratively on the plan step itself (Precondition,
// OnRevise) and via tracked state (LastAsyncStepIdx). Agents drop
// in their own tool catalog and their own plan template; this
// executor just runs the plan.
type StructuredGoalExecutor struct {
	tz *time.Location
}

// NewStructuredGoalExecutor constructs an executor with the given timezone
// for "[current_datetime: ...]" context injection.
func NewStructuredGoalExecutor(tz *time.Location) *StructuredGoalExecutor {
	return &StructuredGoalExecutor{tz: tz}
}

// DefaultTools returns nil — the structured executor uses whatever tool
// allowlist the agent_task itself declares (AgentTask.Tools); the
// scheduler resolves that against the master registry.
func (e *StructuredGoalExecutor) DefaultTools() []string { return nil }

// runPlanExecutor handles goal tasks using the plan-then-execute pattern.
// First iteration: LLM creates a structured plan.
// Subsequent iterations: handler executes steps mechanically, consulting LLM only at decision points.
func (e *StructuredGoalExecutor) Run(ctx context.Context, task core.AgentTask, deps core.AgentDeps) (core.IterationResult, error) {
	// Parse progress.
	var progress goalPlanProgress
	progress.LastAsyncStepIdx = -1
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
		return e.planPhase(ctx, task, deps, &progress, sessID, routerModel, modelRole)
	}

	// Have plan → EXECUTION PHASE.
	return e.executeStep(ctx, task, deps, &progress, sessID, routerModel, modelRole)
}

// planPhase asks the LLM to create a structured plan for the goal.
//
// Planning is a pure single-shot LLM call: no session, no history, no tools,
// no personality prompts, no reflex context injection. The model sees only
// the dedicated goal-plan-system prompt (strict JSON-only instructions) and
// a user message with the goal and available tool names. Session-backed
// agent.Loop would contaminate subsequent plan retries with the model's
// earlier prose-style outputs — single-shot guarantees every retry starts
// from the same clean prompt.
func (e *StructuredGoalExecutor) planPhase(ctx context.Context, task core.AgentTask, deps core.AgentDeps, progress *goalPlanProgress, sessID, model, modelRole string) (core.IterationResult, error) {
	desc := ""
	if task.Description != nil {
		desc = *task.Description
	}

	_ = modelRole // reserved for future per-role prompt selection

	systemPrompt, err := deps.Prompts.Get(ctx, "goal-plan-system")
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("load prompt goal-plan-system: %w", err)
	}
	now := time.Now().In(e.tz)
	systemPrompt = fmt.Sprintf("[current_datetime: %s]\n\n%s", now.Format("2006-01-02 15:04 MST (Monday)"), systemPrompt)

	goalPlanUser, _ := deps.Prompts.Get(ctx, "goal-plan-user")
	planPrompt := fmt.Sprintf(goalPlanUser, task.Title, desc, buildToolsList(deps))

	resp, err := deps.LLM.Complete(ctx, core.CompletionRequest{
		Model:     model,
		MaxTokens: deps.Config.Limits.MaxOutputTokens,
		System:    systemPrompt,
		Messages:  []core.Message{{Role: "user", Content: planPrompt}},
	})
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("planning LLM: %w", err)
	}
	reply := core.ExtractText(resp.Content)

	// Parse plan from LLM output.
	plan, parseErr := parsePlan(reply)
	if parseErr != nil || len(plan) < 4 {
		progress.RetryCount++
		deps.Logger.Warn("plan-executor: invalid plan",
			"error", parseErr, "steps", len(plan), "reply_len", len(reply),
			"attempt", progress.RetryCount, "reply_head", truncate(reply, 200))
		if progress.RetryCount >= maxStepRetries {
			summary := fmt.Sprintf("Goal failed: could not generate a valid plan in %d attempts. Last reply head: %s",
				progress.RetryCount, truncate(reply, 300))
			return core.IterationResult{Done: true, Output: summary, Notify: summary}, nil
		}
		progress.Phase = "plan_invalid"
		progress.Summary = fmt.Sprintf("Plan invalid (attempt %d/%d, %d steps): %s",
			progress.RetryCount, maxStepRetries, len(plan), truncate(reply, 200))
		progressJSON, _ := json.Marshal(progress)
		return core.IterationResult{Progress: progressJSON}, nil
	}
	progress.RetryCount = 0 // plan accepted — reset the counter for execution-phase retries

	// Plan must contain at least one "tool" step so the executor has
	// something to do. Structure-only plans (all decide/wait/milestone)
	// would loop forever.
	hasTool := false
	for _, s := range plan {
		if s.Action == "tool" {
			hasTool = true
			break
		}
	}
	if !hasTool {
		deps.Logger.Warn("plan-executor: plan has no tool steps, retrying")
		progress.Phase = "plan_invalid"
		progress.Summary = "Plan must include at least one tool step"
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
func evalPrecondition(p *PlanPrecondition, ctx map[string]any) string {
	if p == nil {
		return ""
	}
	if len(p.StatusIn) > 0 {
		status, _ := ctx["status"].(string)
		if !stringInSlice(status, p.StatusIn) {
			return fmt.Sprintf("status=%q not in %v", status, p.StatusIn)
		}
	}
	if len(p.AnyPresent) > 0 {
		anyPresent := false
		for _, k := range p.AnyPresent {
			if v, ok := ctx[k]; ok && isPresent(v) {
				anyPresent = true
				break
			}
		}
		if !anyPresent {
			return fmt.Sprintf("none of %v present", p.AnyPresent)
		}
	}
	return ""
}

func stringInSlice(s string, xs []string) bool {
	for _, x := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// isPresent reports whether a JSON-decoded value counts as "present" for
// a precondition check. Empty strings, zero numbers, false bools, nil,
// empty slices/maps all count as absent.
func isPresent(v any) bool {
	switch vv := v.(type) {
	case nil:
		return false
	case string:
		return vv != ""
	case bool:
		return vv
	case float64:
		return vv != 0
	case int:
		return vv != 0
	case []any:
		return len(vv) > 0
	case map[string]any:
		return len(vv) > 0
	}
	return true
}

// formatMetadata renders the top-level scalar fields of a JSON object as a
// compact "Context metadata:\n- key: value\n…" block for the decide prompt.
// Any tool's status payload gets its salient signals surfaced up top, while
// large strings are summarised (not dumped) to keep the truncation budget
// available for the full raw JSON underneath.
//
// Rules for rendering:
//   - strings ≤ 120 chars: printed quoted.
//   - strings > 120 chars: printed as `<key> (string, N chars)`.
//   - bools / numbers / small slices / small maps: printed as-is.
//   - nil values: skipped.
//   - nested objects / large slices: printed as `<key>: <type>`.
func formatMetadata(obj map[string]any) string {
	if len(obj) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Context metadata:\n")
	for k, v := range obj {
		switch vv := v.(type) {
		case nil:
			// skip
		case string:
			if len(vv) == 0 {
				fmt.Fprintf(&b, "- %s: \"\"\n", k)
			} else if len(vv) <= 120 {
				fmt.Fprintf(&b, "- %s: %q\n", k, vv)
			} else {
				fmt.Fprintf(&b, "- %s: (string, %d chars)\n", k, len(vv))
			}
		case bool:
			fmt.Fprintf(&b, "- %s: %t\n", k, vv)
		case float64:
			fmt.Fprintf(&b, "- %s: %v\n", k, vv)
		case int:
			fmt.Fprintf(&b, "- %s: %d\n", k, vv)
		case []any:
			fmt.Fprintf(&b, "- %s: array[%d]\n", k, len(vv))
		case map[string]any:
			fmt.Fprintf(&b, "- %s: object{%d keys}\n", k, len(vv))
		default:
			fmt.Fprintf(&b, "- %s: <%T>\n", k, v)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// substituteVars replaces $peer_task_id and $result.X in input JSON.
func substituteVars(input json.RawMessage, progress *goalPlanProgress) json.RawMessage {
	if len(input) == 0 {
		return input
	}
	s := string(input)
	s = strings.ReplaceAll(s, "$peer_task_id", progress.PeerTaskID)

	// Substitute repo_path variants — LLM writes $result.repo_path, $result.path, etc.
	if progress.RepoPath != "" {
		s = strings.ReplaceAll(s, "$result.repo_path", progress.RepoPath)
		s = strings.ReplaceAll(s, "$result.path", progress.RepoPath)
	}

	// Substitute $result.field references.
	if len(progress.LastResult) > 0 {
		var lastResult map[string]any
		if json.Unmarshal(progress.LastResult, &lastResult) == nil {
			// First pass: exact field match.
			for k, v := range lastResult {
				placeholder := fmt.Sprintf("$result.%s", k)
				if str, ok := v.(string); ok {
					s = strings.ReplaceAll(s, placeholder, str)
				}
			}
			// Second pass: if any $result.* remains unresolved, try to match
			// by suffix (e.g. $result.path → repo_path, $result.id → task_id).
			if strings.Contains(s, "$result.") {
				for k, v := range lastResult {
					str, ok := v.(string)
					if !ok || str == "" {
						continue
					}
					// Check if any unresolved placeholder ends with this key's suffix.
					// E.g. "repo_path" matches "$result.path", "task_id" matches "$result.id".
					for suffix := k; strings.Contains(suffix, "_"); {
						parts := strings.SplitN(suffix, "_", 2)
						suffix = parts[1]
						placeholder := fmt.Sprintf("$result.%s", suffix)
						if strings.Contains(s, placeholder) {
							s = strings.ReplaceAll(s, placeholder, str)
						}
					}
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
