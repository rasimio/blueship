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
	RepoPath    string          `json:"repo_path,omitempty"`
	Plan        []PlanStep      `json:"plan,omitempty"`
	CurrentStep int             `json:"current_step"`
	// RetryCount is the consecutive tool-error count on the current step.
	// Reset to 0 when any tool call succeeds.
	RetryCount int `json:"retry_count,omitempty"`
	// ReviseCount is the number of REVISE decisions issued on this step. It
	// accumulates across the execute→decide→REVISE→rewind→execute cycle,
	// because a successful re-execution does NOT mean the reviewer will
	// approve the next time. Persists separately from RetryCount so that
	// tool retries (which reset on success) don't mask a decide loop.
	ReviseCount int             `json:"revise_count,omitempty"`
	LastResult  json.RawMessage `json:"last_result,omitempty"`
	Phase       string          `json:"phase"`
	Summary     string          `json:"summary"`
}

// maxStepRetries caps how many times we retry any single step (tool error or REVISE)
// before failing the goal. Prevents infinite loops on fatal states like a peer task
// that cannot produce a branch or an LLM that keeps rejecting a correct result.
const maxStepRetries = 3

// StructuredGoalExecutor implements core.GoalHandler for goals whose strategy
// is "structured" (LLM generates a JSON plan up front; executor interprets
// steps mechanically). Currently contains Arlene-specific assumptions about
// code_task_* tool names — those will be lifted out in Phase 2 of the
// migration. For Phase 1 the only change is: this executor operates on
// core.Goal instead of core.AgentTask.
type StructuredGoalExecutor struct {
	tz *time.Location
}

// NewStructuredGoalExecutor constructs an executor with the given timezone
// for "[current_datetime: ...]" context injection.
func NewStructuredGoalExecutor(tz *time.Location) *StructuredGoalExecutor {
	return &StructuredGoalExecutor{tz: tz}
}


// runPlanExecutor handles goal tasks using the plan-then-execute pattern.
// First iteration: LLM creates a structured plan.
// Subsequent iterations: handler executes steps mechanically, consulting LLM only at decision points.
func (e *StructuredGoalExecutor) Run(ctx context.Context, goal core.Goal, deps core.AgentDeps) (core.IterationResult, error) {
	// Parse progress.
	var progress goalPlanProgress
	if len(goal.Progress) > 0 && string(goal.Progress) != "{}" {
		json.Unmarshal(goal.Progress, &progress)
	}

	// Resolve model.
	modelRole := "cortex"
	if goal.Config != nil {
		var roleCfg struct {
			ModelRole string `json:"model_role"`
		}
		if json.Unmarshal(goal.Config, &roleCfg) == nil && roleCfg.ModelRole != "" {
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
		sessID, err = deps.Store.CreateSessionWithSource(ctx, goal.UserID.String(), displayModel, "agent_task", goal.ID.String())
		if err != nil {
			return core.IterationResult{}, fmt.Errorf("create session: %w", err)
		}
		progress.SessionID = sessID
	}

	// No plan yet → PLANNING PHASE.
	if len(progress.Plan) == 0 {
		return e.planPhase(ctx, goal, deps, &progress, sessID, routerModel, modelRole)
	}

	// Have plan → EXECUTION PHASE.
	return e.executeStep(ctx, goal, deps, &progress, sessID, routerModel, modelRole)
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
func (e *StructuredGoalExecutor) planPhase(ctx context.Context, goal core.Goal, deps core.AgentDeps, progress *goalPlanProgress, sessID, model, modelRole string) (core.IterationResult, error) {
	desc := ""
	if goal.Description != nil {
		desc = *goal.Description
	}

	_ = modelRole // reserved for future per-role prompt selection

	systemPrompt, err := deps.Prompts.Get(ctx, "goal-plan-system")
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("load prompt goal-plan-system: %w", err)
	}
	now := time.Now().In(e.tz)
	systemPrompt = fmt.Sprintf("[current_datetime: %s]\n\n%s", now.Format("2006-01-02 15:04 MST (Monday)"), systemPrompt)

	goalPlanUser, _ := deps.Prompts.Get(ctx, "goal-plan-user")
	planPrompt := fmt.Sprintf(goalPlanUser, goal.Title, desc, buildToolsList(deps))

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

	// Validate: plan must contain at least one "tool" step with code_task_create.
	hasTaskCreate := false
	for _, s := range plan {
		if s.Action == "tool" && s.Tool == "code_task_create" {
			hasTaskCreate = true
			break
		}
	}
	if !hasTaskCreate {
		deps.Logger.Warn("plan-executor: plan missing code_task_create, retrying")
		progress.Phase = "plan_invalid"
		progress.Summary = "Plan missing code_task_create step"
		progressJSON, _ := json.Marshal(progress)
		return core.IterationResult{Progress: progressJSON}, nil
	}

	// Ensure code_task_execute exists after first decide (plan review).
	// LLM frequently omits this critical step.
	hasExecute := false
	for _, s := range plan {
		if s.Action == "tool" && s.Tool == "code_task_execute" {
			hasExecute = true
			break
		}
	}
	if !hasExecute {
		// Find first "decide" after code_task_create and inject execute + wait after it.
		pastCreate := false
		for i, s := range plan {
			if s.Action == "tool" && s.Tool == "code_task_create" {
				pastCreate = true
			}
			if pastCreate && s.Action == "decide" {
				// Insert execute + wait after this decide step.
				executeStep := PlanStep{Action: "tool", Tool: "code_task_execute", Input: json.RawMessage(`{"task_id":"$peer_task_id"}`)}
				waitStep := PlanStep{Action: "wait"}
				newPlan := make([]PlanStep, 0, len(plan)+2)
				newPlan = append(newPlan, plan[:i+1]...)
				newPlan = append(newPlan, executeStep, waitStep)
				newPlan = append(newPlan, plan[i+1:]...)
				plan = newPlan
				deps.Logger.Info("plan-executor: injected missing code_task_execute after decide", "at", i+1)
				break
			}
		}
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
func (e *StructuredGoalExecutor) executeStep(ctx context.Context, goal core.Goal, deps core.AgentDeps, progress *goalPlanProgress, sessID, model, modelRole string) (core.IterationResult, error) {
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
		return e.execDecideStep(ctx, goal, deps, progress, step, sessID, model, modelRole)

	case "milestone":
		msg := step.Message
		if msg == "" {
			msg = fmt.Sprintf("%s — milestone reached (step %d/%d)", goal.Title, progress.CurrentStep+1, len(progress.Plan))
		}
		progress.CurrentStep++
		progress.Phase = "milestone"
		progress.Summary = msg
		progressJSON, _ := json.Marshal(progress)
		return core.IterationResult{Pause: true, Progress: progressJSON, Notify: msg}, nil

	case "done":
		// Build structured completion report.
		var report strings.Builder
		report.WriteString(fmt.Sprintf("[DONE] %s\n\n", goal.Title))
		report.WriteString(fmt.Sprintf("Steps completed: %d/%d\n", progress.CurrentStep, len(progress.Plan)))
		report.WriteString(fmt.Sprintf("Iterations used: %d/%d\n", goal.Iteration+1, goal.MaxIterations))
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
		if strings.Contains(result, "already exists") {
			deps.Logger.Info("plan-executor: resource already exists, treating as success", "tool", step.Tool)
			isError = false
			// Try to get repo path from input name via code_repo_list.
			if step.Tool == "code_repo_create" {
				var inp map[string]any
				if json.Unmarshal(input, &inp) == nil {
					if name, ok := inp["name"].(string); ok && name != "" {
						listResult, listErr := deps.Registry.Execute(ctx, "code_repo_list", nil)
						if !listErr {
							// code_repo_list returns {"repos": [...], "count": N}
							var wrapper struct {
								Repos []struct {
									Name string `json:"name"`
									Path string `json:"path"`
								} `json:"repos"`
							}
							if json.Unmarshal([]byte(listResult), &wrapper) == nil {
								for _, r := range wrapper.Repos {
									if r.Name == name && r.Path != "" {
										progress.RepoPath = r.Path
										result = fmt.Sprintf(`{"repo_path":"%s","name":"%s","status":"already_exists"}`, r.Path, name)
										deps.Logger.Info("plan-executor: found repo path from list", "name", name, "path", r.Path)
									}
								}
							}
						}
					}
				}
			}
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

	// Extract known fields from result.
	var resultMap map[string]any
	if json.Unmarshal([]byte(result), &resultMap) == nil {
		if tid, ok := resultMap["task_id"].(string); ok && tid != "" {
			progress.PeerTaskID = tid
		}
		if rp, ok := resultMap["repo_path"].(string); ok && rp != "" {
			progress.RepoPath = rp
		}
		// code_repo_create returns "path" not "repo_path" — extract either.
		if rp, ok := resultMap["path"].(string); ok && rp != "" && progress.RepoPath == "" {
			progress.RepoPath = rp
		}
		// Fallback: if name is present but no path, ask registry for repo info.
		if progress.RepoPath == "" {
			if name, ok := resultMap["name"].(string); ok && name != "" {
				listResult, isErr := deps.Registry.Execute(ctx, "code_repo_list", nil)
				if !isErr {
					var wrapper struct {
						Repos []struct {
							Name string `json:"name"`
							Path string `json:"path"`
						} `json:"repos"`
					}
					if json.Unmarshal([]byte(listResult), &wrapper) == nil {
						for _, r := range wrapper.Repos {
							if r.Name == name && r.Path != "" {
								progress.RepoPath = r.Path
							}
						}
					}
				}
			}
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
func (e *StructuredGoalExecutor) execDecideStep(ctx context.Context, goal core.Goal, deps core.AgentDeps, progress *goalPlanProgress, step PlanStep, sessID, model, modelRole string) (core.IterationResult, error) {
	// Fetch context data if context_tool specified.
	var contextData string
	if step.ContextTool != "" {
		input := substituteVars(json.RawMessage(fmt.Sprintf(`{"task_id":"%s"}`, progress.PeerTaskID)), progress)
		result, isError := deps.Registry.Execute(ctx, step.ContextTool, input)
		if !isError {
			contextData = result
		}
	}

	// When the context comes from code_task_status, extract the key metadata
	// fields up front. If we only hand the reviewer the raw JSON, a huge diff
	// eats the 3000-char truncation window and the reviewer ends up judging
	// the work from a mid-diff fragment — it tends to REVISE "can't verify"
	// even when commit_sha, test_pass=true, and a 36KB diff already prove
	// the code is there. Surfacing the metadata explicitly lets it judge on
	// the facts, not on a truncated string.
	var peer peerTaskStatus
	var peerParsed bool
	if step.ContextTool == "code_task_status" && contextData != "" {
		peerParsed = json.Unmarshal([]byte(contextData), &peer) == nil
	}

	// Fix B: short-circuit auto-REVISE for post-execute decisions when the peer
	// task has no produced code. This prevents the LLM from approving plan_ready
	// state (which means "plan regenerated after revise, but code not re-executed").
	if peerParsed && hasExecuteBeforeStep(progress.Plan, progress.CurrentStep) {
		codeReady := (peer.Status == "done" || peer.Status == "review_ready") &&
			(peer.CommitSHA != "" || peer.Diff != "" || peer.Branch != "")
		if !codeReady {
			autoMsg := fmt.Sprintf("REVISE: post-execute decide requires produced code, but peer task has status=%s, commit=%q, branch=%q",
				peer.Status, peer.CommitSHA, peer.Branch)
			deps.Logger.Info("plan-executor: auto-REVISE (post-execute, no code)",
				"status", peer.Status, "commit", peer.CommitSHA, "branch", peer.Branch)
			return e.handleRevise(ctx, deps, progress, autoMsg)
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

	// When we have structured metadata, prepend it as a compact header so the
	// reviewer sees the signals that don't fit inside a truncated diff. The
	// raw JSON still follows in case there's something the header missed.
	contextBlock := truncate(contextData, 3000)
	if peerParsed {
		contextBlock = formatPeerMetadata(peer) + "\n\nRaw status JSON (truncated):\n" + contextBlock
	}

	decisionPrompt := fmt.Sprintf(decideUser,
		progress.CurrentStep+1, goal.Title,
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

	return e.handleRevise(ctx, deps, progress, reply)
}

// handleRevise processes a REVISE decision: calls code_task_revise with feedback,
// rewinds CurrentStep to the last code_task_execute so the executor re-runs
// execution on the regenerated plan, and enforces a retry cap.
//
// Increments ReviseCount, not RetryCount, because the rewind will cause a
// successful code_task_execute to reset RetryCount to 0 and mask the loop.
func (e *StructuredGoalExecutor) handleRevise(ctx context.Context, deps core.AgentDeps, progress *goalPlanProgress, reply string) (core.IterationResult, error) {
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

	if progress.PeerTaskID != "" {
		input := json.RawMessage(fmt.Sprintf(`{"task_id":"%s","feedback":"%s"}`,
			progress.PeerTaskID, strings.ReplaceAll(feedback, `"`, `\"`)))
		_, _ = deps.Registry.Execute(ctx, "code_task_revise", input)
	}

	// Fix A: rewind CurrentStep to the last code_task_execute step so the
	// executor re-runs execution on the newly regenerated plan. Otherwise
	// the next iteration would re-run decide and approve the plan_ready
	// state without ever invoking the executor on the new plan.
	if rewindTo := findLastExecuteStep(progress.Plan, progress.CurrentStep); rewindTo >= 0 {
		deps.Logger.Info("plan-executor: REVISE rewind",
			"from_step", progress.CurrentStep, "to_step", rewindTo,
			"revise_count", progress.ReviseCount)
		progress.CurrentStep = rewindTo
	}

	progress.Phase = "waiting_for_revise"
	progress.Summary = fmt.Sprintf("Revised (attempt %d): %s", progress.ReviseCount, truncate(feedback, 200))
	progressJSON, _ := json.Marshal(progress)
	return core.IterationResult{Pause: true, Progress: progressJSON}, nil
}

// findLastExecuteStep returns the index of the last code_task_execute step
// at or before `before`. Returns -1 if none found.
func findLastExecuteStep(plan []PlanStep, before int) int {
	if before >= len(plan) {
		before = len(plan) - 1
	}
	for i := before; i >= 0; i-- {
		if plan[i].Action == "tool" && plan[i].Tool == "code_task_execute" {
			return i
		}
	}
	return -1
}

// hasExecuteBeforeStep reports whether any code_task_execute step appears
// strictly before `at` in the plan. Used to distinguish post-execute decides
// (which must verify actual produced code) from pre-execute plan-review decides.
func hasExecuteBeforeStep(plan []PlanStep, at int) bool {
	if at > len(plan) {
		at = len(plan)
	}
	for i := 0; i < at; i++ {
		if plan[i].Action == "tool" && plan[i].Tool == "code_task_execute" {
			return true
		}
	}
	return false
}

// peerTaskStatus mirrors the fields we care about from code_task_status JSON.
// All fields are optional; missing fields are rendered as empty strings or
// zero values and skipped from the metadata header.
type peerTaskStatus struct {
	Status         string `json:"status"`
	Branch         string `json:"branch"`
	CommitSHA      string `json:"commit_sha"`
	Diff           string `json:"diff"`
	TestPass       *bool  `json:"test_pass"`
	BaselineTests  *int   `json:"baseline_tests"`
	PublishStatus  string `json:"publish_status"`
	NumTurns       int    `json:"num_turns"`
	DurationMs     int64  `json:"duration_ms"`
	PlanFeedback   string `json:"plan_feedback"`
	Error          string `json:"error"`
}

// formatPeerMetadata renders peerTaskStatus as a compact human-readable
// header for the decide prompt. Lets the reviewer judge by metadata
// (test_pass, commit exists, diff size) instead of trying to parse a
// truncated diff fragment.
func formatPeerMetadata(p peerTaskStatus) string {
	var b strings.Builder
	b.WriteString("Peer task metadata:\n")
	if p.Status != "" {
		fmt.Fprintf(&b, "- status: %s\n", p.Status)
	}
	if p.Branch != "" {
		fmt.Fprintf(&b, "- branch: %s\n", p.Branch)
	}
	if p.CommitSHA != "" {
		short := p.CommitSHA
		if len(short) > 12 {
			short = short[:12]
		}
		fmt.Fprintf(&b, "- commit_sha: %s\n", short)
	}
	fmt.Fprintf(&b, "- diff_size: %d chars\n", len(p.Diff))
	if p.TestPass != nil {
		fmt.Fprintf(&b, "- test_pass: %t\n", *p.TestPass)
	}
	if p.BaselineTests != nil {
		fmt.Fprintf(&b, "- baseline_tests: %d\n", *p.BaselineTests)
	}
	if p.PublishStatus != "" {
		fmt.Fprintf(&b, "- publish_status: %s\n", p.PublishStatus)
	}
	if p.NumTurns > 0 {
		fmt.Fprintf(&b, "- num_turns: %d\n", p.NumTurns)
	}
	if p.DurationMs > 0 {
		fmt.Fprintf(&b, "- duration: %ds\n", p.DurationMs/1000)
	}
	if p.PlanFeedback != "" {
		fmt.Fprintf(&b, "- plan_feedback: %s\n", truncate(p.PlanFeedback, 200))
	}
	if p.Error != "" {
		fmt.Fprintf(&b, "- error: %s\n", truncate(p.Error, 200))
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
