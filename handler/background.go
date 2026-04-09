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
// Iteration 0 = planning, 1..N-2 = execution, N-1 = synthesis.
type Background struct {
	tz *time.Location
}

func NewBackground(tz *time.Location) *Background {
	return &Background{tz: tz}
}

func (b *Background) DefaultTools() []string {
	return nil
}

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

	// 2. Parse progress (contains session_id for shared session + plan)
	var progress bgProgress
	if len(task.Progress) > 0 && string(task.Progress) != "{}" {
		json.Unmarshal(task.Progress, &progress)
	}

	// 3. Resolve model: router format for LLM, display name for session.
	routerModel := deps.Config.Models.Primary.ForRouter()
	displayModel := deps.Config.Models.Primary.Name
	if deps.ModelStore != nil {
		if m := deps.ModelStore.ForRouter("background"); m != "" {
			routerModel = m
		}
		if ref := deps.ModelStore.Get("background"); ref.Name != "" {
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

	// Build user message. Tasks with a custom prompt (config.prompt) are
	// self-contained — no multi-phase planning/execution/synthesis overlay.
	var msg string
	if instructionKey != "background-task" {
		msg = fmt.Sprintf("[TASK: %s]\n%s", task.Title, desc)
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

	// 6. Inject context (active notes, etc.) if available.
	var injectedCtx string
	if deps.ContextInjector != nil {
		injectedCtx = deps.ContextInjector(ctx, task.UserID.String(), msg)
	}

	// 7. Run agent loop (shared session — LLM sees full history)
	loop := agent.NewLoop(deps.LLM, deps.Store, deps.Registry, deps.RoleTools, deps.Config, deps.Logger)

	reply, err := loop.Run(ctx, agent.RunConfig{
		SessionID:       sessID,
		SystemPrompt:    systemPrompt,
		InjectedContext: injectedCtx,
		Model:           routerModel,
		MaxTokens:       deps.Config.Limits.MaxOutputTokens,
		MaxTurns:        deps.Config.Gateway.MaxTurns,
		Role:            "background",
	}, msg)
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("agent loop: %w", err)
	}

	// 7. Save progress with session ID
	progress.Phase = fmt.Sprintf("iteration_%d", task.Iteration+1)
	progress.Summary = truncate(reply, 500)
	progressJSON, _ := json.Marshal(progress)

	if isLast {
		clean := strings.ReplaceAll(reply, "[DONE]", "")
		clean = strings.ReplaceAll(clean, "[CONTINUE]", "")
		clean = strings.ReplaceAll(clean, "[MILESTONE]", "")
		clean = strings.TrimSpace(clean)

		// Archive session (one-shot, no reuse after task completion).
		deps.Store.ArchiveSession(ctx, sessID)

		// Filter no-op — nothing to report.
		if clean == "" || strings.Contains(clean, "[no-op]") {
			deps.Store.ArchiveSession(ctx, sessID)
			return core.IterationResult{Done: true}, nil
		}
		return core.IterationResult{
			Done:   true,
			Output: clean,
			Notify: clean,
		}, nil
	}

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

// bgProgress extends TaskProgress with session management.
type bgProgress struct {
	core.TaskProgress
	SessionID string `json:"session_id"` // shared session across iterations
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
