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
// Loads "background-task" system prompt, injects task context and progress,
// runs an agent loop, parses LLM response to determine completion.
type Background struct {
	tz *time.Location
}

// NewBackground creates a background task handler.
func NewBackground(tz *time.Location) *Background {
	return &Background{tz: tz}
}

func (b *Background) DefaultTools() []string {
	return nil // determined by task.Tools or role
}

func (b *Background) Run(ctx context.Context, task core.AgentTask, deps core.AgentDeps) (core.IterationResult, error) {
	// 1. Load system prompt: preamble + soul + agents + background-task
	var parts []string
	for _, key := range []string{"preamble", "soul", "agents", "background-task"} {
		p, err := deps.Prompts.Get(ctx, key)
		if err != nil {
			return core.IterationResult{}, fmt.Errorf("load prompt %q: %w", key, err)
		}
		parts = append(parts, p)
	}
	systemPrompt := strings.Join(parts, "\n\n")

	// 2. Inject datetime
	now := time.Now().In(b.tz)
	systemPrompt = fmt.Sprintf("[current_datetime: %s]\n\n%s",
		now.Format("2006-01-02 15:04 MST (Monday)"), systemPrompt)

	// 3. Parse existing progress
	var progress core.TaskProgress
	if len(task.Progress) > 0 && string(task.Progress) != "{}" {
		json.Unmarshal(task.Progress, &progress)
	}

	// 4. Build user message
	desc := ""
	if task.Description != nil {
		desc = *task.Description
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "[BACKGROUND TASK: %s]\n", task.Title)
	fmt.Fprintf(&msg, "Mission: %s\n", desc)
	fmt.Fprintf(&msg, "Iteration: %d/%d\n", task.Iteration+1, task.MaxIterations)
	if task.Deadline != nil {
		fmt.Fprintf(&msg, "Deadline: %s\n", task.Deadline.In(b.tz).Format("2006-01-02 15:04"))
	}

	if progress.Phase != "" {
		msg.WriteString("\n[PREVIOUS PROGRESS]\n")
		fmt.Fprintf(&msg, "Phase: %s\n", progress.Phase)
		if progress.Summary != "" {
			fmt.Fprintf(&msg, "Summary: %s\n", progress.Summary)
		}
		for _, f := range progress.Findings {
			fmt.Fprintf(&msg, "- %s\n", f)
		}
		if len(progress.NextSteps) > 0 {
			msg.WriteString("Next steps:\n")
			for _, s := range progress.NextSteps {
				fmt.Fprintf(&msg, "- %s\n", s)
			}
		}
	}

	// 5. Create session and run agent loop
	sessID, err := deps.Store.CreateSession(ctx, task.UserID.String(), deps.Config.Models.Primary.ForRouter())
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("create session: %w", err)
	}

	// Resolve model: prefer "background" role from ModelStore, fallback to primary.
	model := deps.Config.Models.Primary.ForRouter()
	if deps.ModelStore != nil {
		if m := deps.ModelStore.ForRouter("background"); m != "" {
			model = m
		}
	}

	loop := agent.NewLoop(deps.LLM, deps.Store, deps.Registry, deps.RoleTools, deps.Config, deps.Logger)

	reply, err := loop.Run(ctx, agent.RunConfig{
		SessionID:    sessID,
		SystemPrompt: systemPrompt,
		Model:        model,
		MaxTokens:    deps.Config.Limits.MaxOutputTokens,
		MaxTurns:     deps.Config.Gateway.MaxTurns,
		Role:         "background",
	}, msg.String())
	if err != nil {
		return core.IterationResult{}, fmt.Errorf("agent loop: %w", err)
	}

	// 6. Parse response
	// Only finish on the last iteration. LLM may output [DONE] early — ignore it.
	isLastIteration := task.MaxIterations > 0 && task.Iteration+1 >= task.MaxIterations
	isDone := isLastIteration

	if isDone {
		clean := strings.ReplaceAll(reply, "[DONE]", "")
		clean = strings.ReplaceAll(clean, "[CONTINUE]", "")
		clean = strings.ReplaceAll(clean, "[MILESTONE]", "")
		clean = strings.TrimSpace(clean)
		return core.IterationResult{
			Done:   true,
			Output: clean,
			Notify: clean,
		}, nil
	}

	// Continue: update progress
	newProgress := core.TaskProgress{
		Phase:    "in_progress",
		Findings: progress.Findings,
		Summary:  truncate(reply, 500),
	}
	newProgress.Findings = append(newProgress.Findings, truncate(reply, 300))
	progressJSON, _ := json.Marshal(newProgress)

	var notify string
	if strings.Contains(reply, "[MILESTONE]") {
		notify = fmt.Sprintf("📋 %s (iteration %d/%d)\n\n%s",
			task.Title, task.Iteration+1, task.MaxIterations, truncate(reply, 400))
	}

	return core.IterationResult{
		Done:     false,
		Progress: progressJSON,
		Notify:   notify,
	}, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
