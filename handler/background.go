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
	// 1. Load system prompt
	var parts []string
	for _, key := range []string{"preamble", "soul", "agents", "background-task"} {
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

	// 4. Get or create shared session (one session for the entire task lifecycle)
	sessID := progress.SessionID
	if sessID == "" {
		var err error
		sessID, err = deps.Store.CreateSessionWithSource(ctx, task.UserID.String(), displayModel, "agent_task", task.ID.String())
		if err != nil {
			return core.IterationResult{}, fmt.Errorf("create session: %w", err)
		}
		progress.SessionID = sessID
	}

	// 5. Build user message based on iteration phase
	desc := ""
	if task.Description != nil {
		desc = *task.Description
	}

	isFirst := task.Iteration == 0
	isLast := task.MaxIterations > 0 && task.Iteration+1 >= task.MaxIterations

	var msg string
	if isFirst {
		// Planning iteration: ask LLM to create a plan
		msg = fmt.Sprintf(
			"[TASK: %s]\nMission: %s\nTotal iterations: %d\n\n"+
				"This is iteration 1/%d — PLANNING.\n"+
				"Create a detailed research plan. Break the mission into %d steps (one per iteration).\n"+
				"Then execute step 1. Use web_search and web_fetch to find real data.\n"+
				"Save important findings via memory_save.",
			task.Title, desc, task.MaxIterations,
			task.MaxIterations, task.MaxIterations-1)
	} else if isLast {
		// Synthesis iteration
		msg = fmt.Sprintf(
			"This is the FINAL iteration %d/%d — SYNTHESIS.\n"+
				"Review everything found in previous iterations (it's all in our conversation history above).\n"+
				"Write a comprehensive final report. Be specific, cite sources.\n"+
				"Save the final summary via memory_save.",
			task.Iteration+1, task.MaxIterations)
	} else {
		// Execution iteration
		msg = fmt.Sprintf(
			"This is iteration %d/%d — EXECUTION.\n"+
				"Continue the research plan from previous iterations.\n"+
				"Look at what was already done above and do the NEXT step.\n"+
				"Use web_search and web_fetch for fresh data. Save findings via memory_save.",
			task.Iteration+1, task.MaxIterations)
	}

	// 6. Run agent loop (shared session — LLM sees full history)
	loop := agent.NewLoop(deps.LLM, deps.Store, deps.Registry, deps.RoleTools, deps.Config, deps.Logger)

	reply, err := loop.Run(ctx, agent.RunConfig{
		SessionID:    sessID,
		SystemPrompt: systemPrompt,
		Model:        routerModel,
		MaxTokens:    deps.Config.Limits.MaxOutputTokens,
		MaxTurns:     deps.Config.Gateway.MaxTurns,
		Role:         "background",
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
