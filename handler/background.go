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

// Background implements core.AgentHandler for generic multi-iteration tasks.
// Uses a shared session across all iterations so LLM has full conversation history.
// Supports auto-pause: if the LLM calls an async peer tool (code_task_create, etc.),
// the handler pauses until an external callback wakes the task.
type Background struct {
	tz *time.Location
}

func NewBackground(tz *time.Location) *Background {
	return &Background{tz: tz}
}

func (b *Background) DefaultTools() []string {
	return nil
}

// pauseTools are async peer tools that require waiting for a callback.
var pauseTools = map[string]bool{
	"code_task_create":  true,
	"code_task_execute": true,
	"code_task_publish": true,
	"code_task_revise":  true,
}

const maxRevisions = 3

func (b *Background) Run(ctx context.Context, task core.AgentTask, deps core.AgentDeps) (core.IterationResult, error) {
	// 1. Load system prompt.
	// Task config may override the instruction prompt key (default: "background-task").
	instructionKey := "background-task"
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

	now := time.Now().In(b.tz)
	systemPrompt = fmt.Sprintf("[current_datetime: %s]\n\n%s",
		now.Format("2006-01-02 15:04 MST (Monday)"), systemPrompt)

	// 2. Parse progress (contains session_id for shared session + pause state)
	var progress bgProgress
	if len(task.Progress) > 0 && string(task.Progress) != "{}" {
		json.Unmarshal(task.Progress, &progress)
	}

	// 3. Resolve model: router format for LLM, display name for session.
	// Task config may override the model role (default: "background").
	modelRole := "background"
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

	// 4. Get or create session.
	// Recurring tasks (schedule != "") get a fresh session each iteration
	// to prevent unbounded history growth. Non-recurring tasks share a session
	// across iterations so the LLM sees full context.
	sessID := progress.SessionID
	if sessID == "" || task.Schedule != nil {
		var err error
		sessID, err = deps.Store.CreateSessionWithSource(ctx, task.UserID.String(), displayModel, "agent_task", task.ID.String())
		if err != nil {
			return core.IterationResult{}, fmt.Errorf("create session: %w", err)
		}
		progress.SessionID = sessID
	}
	// Recurring tasks: archive session when done (progress is reset between runs).
	// Use background context — parent ctx may be cancelled on shutdown.
	if task.Schedule != nil {
		defer deps.Store.ArchiveSession(context.Background(), sessID)
	}

	// 5. Build user message based on iteration phase
	desc := ""
	if task.Description != nil {
		desc = *task.Description
	}

	isLast := task.MaxIterations > 0 && task.Iteration+1 >= task.MaxIterations

	var msg string

	// Resumed from pause — tell LLM what woke it + last progress summary.
	if progress.PeerTaskID != "" && progress.Phase == "waiting" {
		resumeMsg := fmt.Sprintf("[RESUME] You were paused waiting for peer task %s. Check its current status and decide next steps.",
			progress.PeerTaskID)
		if progress.Summary != "" {
			resumeMsg += fmt.Sprintf("\n\nLast progress: %s", progress.Summary)
		}
		msg = fmt.Sprintf("%s\nIteration: %d/%d", resumeMsg, task.Iteration+1, task.MaxIterations)
	} else if instructionKey != "background-task" {
		// Tasks with a custom prompt (config.prompt) are self-contained —
		// no multi-phase planning/execution/synthesis overlay.
		msg = fmt.Sprintf("[TASK: %s]\n%s\nIteration: %d/%d",
			task.Title, desc, task.Iteration+1, task.MaxIterations)
	} else {
		isFirst := task.Iteration == 0

		phaseKey := "background-execution"
		if isFirst {
			phaseKey = "background-planning"
		} else if isLast {
			phaseKey = "background-synthesis"
		}
		phasePrompt, _ := deps.Prompts.Get(ctx, phaseKey)

		msg = fmt.Sprintf("[TASK: %s]\nMission: %s\nIteration: %d/%d\n\n%s",
			task.Title, desc, task.Iteration+1, task.MaxIterations, phasePrompt)
	}

	// Budget warning.
	remaining := task.MaxIterations - (task.Iteration + 1)
	if remaining <= 3 && remaining > 0 {
		msg += fmt.Sprintf("\n\nLow iteration budget: %d remaining.", remaining)
	}

	// 6. Inject context (active notes, etc.) if available.
	var injectedCtx string
	if deps.ContextInjector != nil {
		injectedCtx = deps.ContextInjector(ctx, task.UserID.String(), msg)
	}

	// 7. Run agent loop with tool tracing and compaction.
	loop := agent.NewLoop(deps.LLM, deps.Store, deps.Registry, deps.RoleTools, deps.Config, deps.Logger)
	loop.SetCompactor(agent.NewCompactor(deps.LLM, deps.Config, deps.Logger))

	result, err := loop.RunTracked(ctx, agent.RunConfig{
		SessionID:       sessID,
		SystemPrompt:    systemPrompt,
		InjectedContext: injectedCtx,
		Model:           routerModel,
		MaxTokens:       deps.Config.Limits.MaxOutputTokens,
		MaxTurns:        deps.Config.Gateway.MaxTurns,
		Role:            modelRole,
	}, msg)
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("agent loop: %w", err)
	}

	reply := result.Text

	// 8. Scan tool traces for async peer tools and revision tracking.
	var peerTaskID string
	calledRevise := false

	for _, trace := range result.ToolTraces {
		if pauseTools[trace.Name] {
			var out map[string]any
			if json.Unmarshal([]byte(trace.Output), &out) == nil {
				if tid, ok := out["task_id"].(string); ok && tid != "" {
					peerTaskID = tid
				}
			}
		}
		if trace.Name == "code_task_revise" {
			calledRevise = true
		}
	}

	// 9. Update revision tracking.
	if peerTaskID != "" && peerTaskID != progress.PeerTaskID {
		progress.RevisionCount = 0
	}
	if calledRevise {
		progress.RevisionCount++
	}

	// 10. Save progress with session ID.
	progress.Phase = fmt.Sprintf("iteration_%d", task.Iteration+1)
	progress.Summary = truncate(reply, 500)
	if peerTaskID != "" {
		progress.PeerTaskID = peerTaskID
	}

	// 11. Check for [DONE].
	if strings.Contains(reply, "[DONE]") || isLast {
		clean := strings.ReplaceAll(reply, "[DONE]", "")
		clean = strings.ReplaceAll(clean, "[CONTINUE]", "")
		clean = strings.ReplaceAll(clean, "[PAUSE]", "")
		clean = strings.ReplaceAll(clean, "[MILESTONE]", "")
		clean = strings.TrimSpace(clean)

		// Archive session (one-shot, no reuse after task completion).
		deps.Store.ArchiveSession(ctx, sessID)

		// Filter no-op and garbage output (e.g. raw UUIDs from tool results).
		if clean == "" || strings.Contains(clean, "[no-op]") || isGarbageOutput(clean) {
			return core.IterationResult{Done: true}, nil
		}
		return core.IterationResult{
			Done:   true,
			Output: clean,
			Notify: clean,
		}, nil
	}

	// 12. Revision cap — escalate if stuck in error loop.
	if progress.RevisionCount >= maxRevisions {
		progress.Phase = "error_loop"
		progressJSON, _ := json.Marshal(progress)

		return core.IterationResult{
			Pause:    true,
			Progress: progressJSON,
			Notify: fmt.Sprintf("%s — peer task %s failed %d times. Need human input.\n\n%s",
				task.Title, progress.PeerTaskID, progress.RevisionCount, truncate(reply, 300)),
		}, nil
	}

	// 13. Determine if we should pause (async peer tool was called or explicit [PAUSE]).
	shouldPause := peerTaskID != "" || strings.Contains(reply, "[PAUSE]")

	if shouldPause {
		progress.Phase = "waiting"
		progressJSON, _ := json.Marshal(progress)

		var notify string
		if strings.Contains(reply, "[MILESTONE]") {
			notify = fmt.Sprintf("%s (iteration %d/%d)\n\n%s",
				task.Title, task.Iteration+1, task.MaxIterations, truncate(reply, 400))
		}

		return core.IterationResult{
			Pause:    true,
			Progress: progressJSON,
			Notify:   notify,
		}, nil
	}

	// 14. No pause — continue to next iteration.
	progressJSON, _ := json.Marshal(progress)

	var notify string
	if strings.Contains(reply, "[MILESTONE]") {
		notify = fmt.Sprintf("%s (iteration %d/%d)\n\n%s",
			task.Title, task.Iteration+1, task.MaxIterations, truncate(reply, 400))
	}

	return core.IterationResult{
		Done:     false,
		Progress: progressJSON,
		Notify:   notify,
	}, nil
}

// bgProgress extends TaskProgress with session management and pause state.
type bgProgress struct {
	core.TaskProgress
	SessionID     string `json:"session_id"`              // shared session across iterations
	PeerTaskID    string `json:"peer_task_id,omitempty"`   // async peer task being awaited
	RevisionCount int    `json:"revision_count,omitempty"` // consecutive revisions for same peer task
}

// isGarbageOutput detects raw tool output that shouldn't be sent to users
// (e.g. bare UUID lists, JSON fragments from tool results).
func isGarbageOutput(s string) bool {
	s = strings.TrimSpace(s)
	// Strip commas, spaces, brackets, newlines — if only UUIDs remain, it's garbage.
	cleaned := strings.NewReplacer(",", "", " ", "", "\n", "", "[", "", "]", "").Replace(s)
	// Check if it's just concatenated UUIDs (36 chars each: 8-4-4-4-12)
	if len(cleaned) > 0 && len(cleaned)%36 == 0 {
		allHex := true
		for _, c := range cleaned {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || c == '-') {
				allHex = false
				break
			}
		}
		if allHex {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
