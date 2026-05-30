package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rasimio/blueship/internal/core"
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
//	StatusIn   — the result's `status` field must equal one of these.
//	AnyPresent — at least one of these result keys must be non-empty
//	             (any scalar != "" / != 0 / != null / != false).
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
