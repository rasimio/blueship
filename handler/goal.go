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

// Goal implements core.AgentHandler for autonomous goal orchestration.
// It drives a peer agent (e.g. Liya) through create→plan→execute→push→PR→merge,
// pausing between async steps and resuming on A2A callbacks.
type Goal struct {
	tz *time.Location
}

func NewGoal(tz *time.Location) *Goal {
	return &Goal{tz: tz}
}

func (g *Goal) DefaultTools() []string {
	return nil // all tools available (including remote code_task_* from peers)
}

// goalProgress persists state between iterations.
type goalProgress struct {
	SessionID     string `json:"session_id"`
	LiyaTaskID    string `json:"liya_task_id,omitempty"`
	Phase         string `json:"phase"`
	Summary       string `json:"summary"`
	RevisionCount int    `json:"revision_count"`
}

// pauseTools are async peer tools that require waiting for a callback.
var pauseTools = map[string]bool{
	"code_task_create":  true,
	"code_task_execute": true,
	"code_task_publish": true,
	"code_task_revise":  true,
}

const maxRevisions = 3

func (g *Goal) Run(ctx context.Context, task core.AgentTask, deps core.AgentDeps) (core.IterationResult, error) {
	// 1. Load system prompt.
	instructionKey := "goal-orchestrator"
	if task.Config != nil {
		var cfg struct {
			Prompt string `json:"prompt"`
		}
		if json.Unmarshal(task.Config, &cfg) == nil && cfg.Prompt != "" {
			instructionKey = cfg.Prompt
		}
	}

	var parts []string
	promptKeys := append(deps.Config.SystemPromptKeys, instructionKey)
	for _, key := range promptKeys {
		p, err := deps.Prompts.Get(ctx, key)
		if err != nil {
			return core.IterationResult{}, fmt.Errorf("load prompt %q: %w", key, err)
		}
		parts = append(parts, p)
	}
	systemPrompt := strings.Join(parts, "\n\n")

	now := time.Now().In(g.tz)
	systemPrompt = fmt.Sprintf("[current_datetime: %s]\n\n%s",
		now.Format("2006-01-02 15:04 MST (Monday)"), systemPrompt)

	// 2. Parse progress.
	var progress goalProgress
	if len(task.Progress) > 0 && string(task.Progress) != "{}" {
		json.Unmarshal(task.Progress, &progress)
	}

	// 3. Resolve model.
	modelRole := "goal"
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

	// 4. Get or create session (always shared across iterations).
	sessID := progress.SessionID
	if sessID == "" {
		var err error
		sessID, err = deps.Store.CreateSessionWithSource(ctx, task.UserID.String(), displayModel, "agent_task", task.ID.String())
		if err != nil {
			return core.IterationResult{}, fmt.Errorf("create session: %w", err)
		}
		progress.SessionID = sessID
	}

	// 5. Build user message.
	desc := ""
	if task.Description != nil {
		desc = *task.Description
	}

	var msg string
	if task.Iteration == 0 {
		// First iteration: present the goal.
		msg = fmt.Sprintf("[GOAL: %s]\n%s\nIteration: %d/%d",
			task.Title, desc, task.Iteration+1, task.MaxIterations)
	} else if progress.LiyaTaskID != "" {
		// Resumed after pause: tell LLM what woke it.
		msg = fmt.Sprintf("[RESUME] You were paused waiting for Liya task %s. Check its current status with code_task_status and decide next steps.\nIteration: %d/%d",
			progress.LiyaTaskID, task.Iteration+1, task.MaxIterations)
	} else {
		// Continuation without a tracked Liya task.
		msg = fmt.Sprintf("[GOAL: %s]\nContinue working on your goal.\nIteration: %d/%d",
			task.Title, task.Iteration+1, task.MaxIterations)
	}

	// Budget warning.
	remaining := task.MaxIterations - (task.Iteration + 1)
	if remaining <= 3 && remaining > 0 {
		msg += fmt.Sprintf("\n\n⚠ Low iteration budget: %d remaining. Wrap up or notify the user.", remaining)
	}

	// 6. Inject context (AME traces, active notes, etc.).
	var injectedCtx string
	if deps.ContextInjector != nil {
		injectedCtx = deps.ContextInjector(ctx, task.UserID.String(), msg)
	}

	// 7. Run agent loop with tool tracing.
	loop := agent.NewLoop(deps.LLM, deps.Store, deps.Registry, deps.RoleTools, deps.Config, deps.Logger)

	result, err := loop.RunTracked(ctx, agent.RunConfig{
		SessionID:       sessID,
		SystemPrompt:    systemPrompt,
		InjectedContext: injectedCtx,
		Model:           routerModel,
		MaxTokens:       deps.Config.Limits.MaxOutputTokens,
		MaxTurns:        deps.Config.Gateway.MaxTurns,
		Role:            "goal",
	}, msg)
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("agent loop: %w", err)
	}

	reply := result.Text

	// 8. Check for [DONE].
	if strings.Contains(reply, "[DONE]") {
		clean := strings.ReplaceAll(reply, "[DONE]", "")
		clean = strings.ReplaceAll(clean, "[PAUSE]", "")
		clean = strings.ReplaceAll(clean, "[MILESTONE]", "")
		clean = strings.TrimSpace(clean)

		deps.Store.ArchiveSession(context.Background(), sessID)

		if clean == "" || isGarbageOutput(clean) {
			return core.IterationResult{Done: true}, nil
		}
		return core.IterationResult{
			Done:   true,
			Output: clean,
			Notify: clean,
		}, nil
	}

	// 9. Scan tool traces for pause-triggering tools and revision tracking.
	var liyaTaskID string
	calledRevise := false

	for _, trace := range result.ToolTraces {
		if pauseTools[trace.Name] {
			var out map[string]any
			if json.Unmarshal([]byte(trace.Output), &out) == nil {
				if tid, ok := out["task_id"].(string); ok && tid != "" {
					liyaTaskID = tid
				}
			}
		}
		if trace.Name == "code_task_revise" {
			calledRevise = true
		}
	}

	// 10. Update revision tracking.
	if liyaTaskID != "" && liyaTaskID != progress.LiyaTaskID {
		// New Liya task — reset revision counter.
		progress.RevisionCount = 0
	}
	if calledRevise {
		progress.RevisionCount++
	}

	// 11. Revision cap — escalate if stuck in error loop.
	if progress.RevisionCount >= maxRevisions {
		progress.LiyaTaskID = liyaTaskID
		progress.Phase = "error_loop"
		progress.Summary = truncate(reply, 500)
		progressJSON, _ := json.Marshal(progress)

		return core.IterationResult{
			Pause:    true,
			Progress: progressJSON,
			Notify: fmt.Sprintf("[GOAL] %s — Liya failed %d times on task %s. Need human input.\n\n%s",
				task.Title, progress.RevisionCount, progress.LiyaTaskID, truncate(reply, 300)),
		}, nil
	}

	// 12. Determine if we should pause (async Liya tool was called) or explicit [PAUSE].
	shouldPause := liyaTaskID != "" || strings.Contains(reply, "[PAUSE]")

	if shouldPause {
		if liyaTaskID != "" {
			progress.LiyaTaskID = liyaTaskID
		}
		progress.Phase = "waiting"
		progress.Summary = truncate(reply, 500)
		progressJSON, _ := json.Marshal(progress)

		var notify string
		if strings.Contains(reply, "[MILESTONE]") {
			notify = fmt.Sprintf("[GOAL] %s (iteration %d/%d)\n\n%s",
				task.Title, task.Iteration+1, task.MaxIterations, truncate(reply, 400))
		}

		return core.IterationResult{
			Pause:    true,
			Progress: progressJSON,
			Notify:   notify,
		}, nil
	}

	// 13. No pause — continue to next iteration (LLM did sync work).
	progress.Phase = fmt.Sprintf("iteration_%d", task.Iteration+1)
	progress.Summary = truncate(reply, 500)
	progressJSON, _ := json.Marshal(progress)

	var notify string
	if strings.Contains(reply, "[MILESTONE]") {
		notify = fmt.Sprintf("[GOAL] %s (iteration %d/%d)\n\n%s",
			task.Title, task.Iteration+1, task.MaxIterations, truncate(reply, 400))
	}

	return core.IterationResult{
		Done:     false,
		Progress: progressJSON,
		Notify:   notify,
	}, nil
}
