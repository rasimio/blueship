package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

// graphPlanMaxTokens fits a full 12-step plan. At effort high the planner
// writes detailed goals/acceptance and 4096 tokens ended mid-plan, before the
// finalizer (production, 2026-09-23); effort low used ~1600.
const graphPlanMaxTokens = 16384

// PlanGraph uses a host-owned DB prompt and a short tool catalog; planning
// cannot execute tools or run research. Context is a persisted task snapshot.
func (b *Background) PlanGraph(ctx context.Context, task core.AgentTask, deps core.AgentDeps, snapshot string) ([]core.TaskStep, error) {
	if deps.Prompts == nil || deps.LLM == nil || deps.Config == nil || deps.Registry == nil {
		return nil, fmt.Errorf("graph planner dependencies unavailable")
	}
	prompt, err := deps.Prompts.Get(ctx, "background-graph-plan")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("background-graph-plan prompt is empty")
	}
	model := deps.Config.Models.Primary
	if deps.ModelStore != nil {
		if ref := deps.ModelStore.Get(modelRoleForTask(task)); ref.Name != "" {
			model = ref
		}
	}
	if model.ForRouter() == "" {
		return nil, fmt.Errorf("graph planner model unavailable")
	}
	type toolInfo struct {
		Name     string `json:"name"`
		ReadOnly bool   `json:"read_only"`
	}
	catalog := []toolInfo{}
	for _, def := range deps.Registry.Definitions() {
		catalog = append(catalog, toolInfo{def.Name, deps.Registry.IsReadOnly(def.Name)})
	}
	researchDeadline := core.TaskResearchDeadline(task, deps.Config.Timeouts.TaskFinalizeReserve)
	var remaining *int64
	if researchDeadline != nil {
		value := max(int64(0), int64(time.Until(*researchDeadline).Seconds()))
		remaining = &value
	}
	input, err := json.Marshal(struct {
		ResearchDeadline         *time.Time `json:"research_deadline,omitempty"`
		ResearchSecondsRemaining *int64     `json:"research_seconds_remaining,omitempty"`
		MaxParallelReads         int        `json:"max_parallel_read_steps"`
		MaxToolTurnsPerStep      int        `json:"max_tool_turns_per_step"`
		CurrentDatetime          string     `json:"current_datetime"`
		Title                    string     `json:"title"`
		Description              *string    `json:"description,omitempty"`
		Acceptance               *string    `json:"acceptance,omitempty"`
		Context                  string     `json:"context,omitempty"`
		Tools                    []toolInfo `json:"tools"`
	}{researchDeadline, remaining, core.BackgroundParallelReads, graphToolTurnLimit(deps), b.graphCurrentDatetime(ctx, deps), task.Title, task.Description, task.AcceptanceCriteria, snapshot, catalog})
	if err != nil {
		return nil, err
	}
	complete := func(messages []core.Message) (string, error) {
		response, err := deps.LLM.Complete(ctx, core.CompletionRequest{Model: model.ForRouter(), System: prompt, Messages: messages, MaxTokens: graphPlanMaxTokens, Temperature: 0.01, Effort: model.Effort, ThinkingMode: model.ThinkingMode})
		if err != nil {
			return "", err
		}
		if response == nil {
			return "", fmt.Errorf("graph planner returned no response")
		}
		// Not retryable: the same request truncates the same way.
		if response.StopReason == "max_tokens" {
			return "", fmt.Errorf("graph planner output token limit reached")
		}
		var text strings.Builder
		for _, block := range response.Content {
			if block.Type == "thinking" || block.Type == "redacted_thinking" {
				continue
			}
			if block.Type != "text" {
				return "", fmt.Errorf("graph planner returned non-text content")
			}
			text.WriteString(block.Text)
		}
		return text.String(), nil
	}
	messages := []core.Message{{Role: "user", Content: core.NormalizeContent(string(input))}}
	text, err := complete(messages)
	if err != nil {
		return nil, err
	}
	steps, planErr := parseGraphPlan(text, deps.Registry)
	if planErr == nil {
		return steps, nil
	}
	// One correction round: a planner slip (no finalizer, an unknown tool)
	// otherwise ends the task before any research (production, 2026-09-23).
	messages = append(messages, core.Message{Role: "assistant", Content: core.NormalizeContent(text)},
		core.Message{Role: "user", Content: core.NormalizeContent("The executor rejected this plan: " + planErr.Error() + ". Return the complete corrected plan as one JSON object in the same format.")})
	if text, err = complete(messages); err != nil {
		return nil, err
	}
	return parseGraphPlan(text, deps.Registry)
}

func parseGraphPlan(raw string, registry *core.ToolRegistry) ([]core.TaskStep, error) {
	var plan struct {
		Steps []struct {
			ID           string   `json:"id"`
			Goal         string   `json:"goal"`
			Acceptance   string   `json:"acceptance"`
			Kind         string   `json:"kind"`
			Dependencies []string `json:"dependencies"`
			Tools        []string `json:"tools"`
		} `json:"steps"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		// %v, not %w: a decoder's io.ErrUnexpectedEOF is malformed model
		// output, not the transport failure TaskRetryPolicy retries.
		return nil, fmt.Errorf("invalid graph plan: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("graph plan contains trailing content")
	}
	if len(plan.Steps) > 12 {
		return nil, fmt.Errorf("graph plan exceeds 12 steps")
	}
	known := map[string]bool{}
	for _, tool := range registry.Definitions() {
		known[tool.Name] = true
	}
	steps := make([]core.TaskStep, 0, len(plan.Steps))
	final := ""
	byID := map[string]core.TaskStep{}
	for _, s := range plan.Steps {
		// Search returns navigation, not source evidence. A research worker
		// must be able to inspect a discovered source when fetching is already
		// authorized by the task registry. Never expand the task's permissions.
		if s.Kind == "read" && slices.Contains(s.Tools, "browser_search") && !slices.Contains(s.Tools, "browser_fetch") && known["browser_fetch"] && registry.IsReadOnly("browser_fetch") {
			s.Tools = append(s.Tools, "browser_fetch")
		}
		for _, name := range s.Tools {
			if !known[name] {
				return nil, fmt.Errorf("unknown graph tool %q", name)
			}
			if s.Kind == "read" && !registry.IsReadOnly(name) {
				return nil, fmt.Errorf("read step %q includes effectful tool %q", s.ID, name)
			}
			if s.Kind == "finalize" && registry.IsReadOnly(name) {
				return nil, fmt.Errorf("finalization cannot restart research")
			}
		}
		if s.Kind == "finalize" {
			if final != "" {
				return nil, fmt.Errorf("graph has multiple finalizers")
			}
			final = s.ID
		}
		step := core.TaskStep{ID: s.ID, Goal: s.Goal, Acceptance: s.Acceptance, Kind: s.Kind, Dependencies: s.Dependencies, Tools: s.Tools, MaxAttempts: 3}
		steps = append(steps, step)
		byID[s.ID] = step
	}
	if err := core.ValidateTaskSteps(steps); err != nil {
		return nil, err
	}
	if final == "" {
		return nil, fmt.Errorf("graph requires a finalizer")
	}
	ancestors := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if ancestors[id] {
			return
		}
		ancestors[id] = true
		for _, dep := range byID[id].Dependencies {
			visit(dep)
		}
	}
	visit(final)
	if len(ancestors) != len(steps) {
		return nil, fmt.Errorf("finalizer must depend on every work branch")
	}
	return steps, nil
}

// graphCurrentDatetime supplies the same owner-local clock as interactive and
// legacy task execution. Source publication dates are not the current date.
func (b *Background) graphCurrentDatetime(ctx context.Context, deps core.AgentDeps) string {
	location := deps.Config.Gateway.TimezoneFor(ctx, b.tz)
	if location == nil {
		location = time.UTC
	}
	return time.Now().In(location).Format(time.RFC3339)
}
