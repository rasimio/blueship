package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/rasimio/blueship/internal/core"
)

// RepairGraphResult revises only the report. It cannot repeat external actions
// or gather new evidence; the result must pass the normal final gates again.
func (b *Background) RepairGraphResult(ctx context.Context, task core.AgentTask, steps []core.TaskStep, deps core.AgentDeps, draft, feedback string) (string, error) {
	if deps.Prompts == nil || deps.LLM == nil || deps.Config == nil {
		return "", fmt.Errorf("graph repair dependencies unavailable")
	}
	prompt, err := deps.Prompts.Get(ctx, "background-graph-repair")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("graph repair prompt missing")
	}
	model := deps.Config.Models.Primary
	if deps.ModelStore != nil {
		if ref := deps.ModelStore.Get(modelRoleForTask(task)); ref.Name != "" {
			model = ref
		}
	}
	confirmed := map[string]string{}
	for _, step := range steps {
		if step.Status == "done" && step.Kind != "finalize" {
			confirmed[step.ID] = step.Goal + "\n" + step.Result
		}
	}
	input, err := json.Marshal(map[string]any{"current_datetime": b.graphCurrentDatetime(ctx, deps), "task": task.Title, "description": task.Description, "acceptance": task.AcceptanceCriteria, "confirmed_results": confirmed, "draft": draft, "feedback": feedback})
	if err != nil {
		return "", err
	}
	response, handled, err := graphRepairEvidence(ctx, task, deps, model, prompt, string(input))
	if !handled {
		response, err = deps.LLM.Complete(ctx, core.CompletionRequest{Model: model.ForRouter(), System: prompt, Messages: []core.Message{{Role: "user", Content: core.NormalizeContent(string(input))}}, MaxTokens: 16384, Effort: model.Effort, ThinkingMode: model.ThinkingMode})
	}
	if err != nil {
		return "", err
	}
	if response == nil || response.StopReason == "max_tokens" {
		return "", fmt.Errorf("empty graph repair response")
	}
	var body strings.Builder
	for _, block := range response.Content {
		if block.Type == "thinking" || block.Type == "redacted_thinking" {
			continue
		}
		if block.Type != "text" {
			return "", fmt.Errorf("graph repair requested a tool")
		}
		body.WriteString(block.Text)
	}
	result, err := applyGraphRepairEdits(draft, body.String())
	if err != nil {
		return "", err
	}
	if task.ExecutorVersion == 2 {
		result, err = renderGraphReport(ctx, task, deps, result)
		if err != nil {
			return "", err
		}
	}
	result = strings.TrimSpace(stripEvidenceMarkers(stripPlanMarkers(scratchpadRE.ReplaceAllString(result, ""))))
	if result == "" {
		return "", fmt.Errorf("empty graph repair candidate")
	}
	return result, nil
}

// Resolve every edit against the original draft, so edits cannot accidentally
// match text introduced by another edit. Invalid patches leave the draft intact.
func applyGraphRepairEdits(draft, patch string) (string, error) {
	var payload struct {
		Edits []struct {
			Before string  `json:"before"`
			After  *string `json:"after"`
			Reason string  `json:"reason,omitempty"` // Optional explanation never affects application.
		} `json:"edits"`
	}
	decoder := json.NewDecoder(strings.NewReader(patch))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return "", fmt.Errorf("invalid graph repair patch: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", fmt.Errorf("graph repair patch has trailing data")
	}
	if len(payload.Edits) == 0 || len(payload.Edits) > 20 {
		return "", fmt.Errorf("graph repair requires 1 to 20 edits")
	}
	type replacement struct {
		start, end int
		text       string
	}
	edits := make([]replacement, 0, len(payload.Edits))
	for _, edit := range payload.Edits {
		start := strings.Index(draft, edit.Before)
		if edit.Before == "" || edit.After == nil || start < 0 || strings.Contains(draft[start+1:], edit.Before) {
			return "", fmt.Errorf("graph repair edit requires a unique original passage and replacement")
		}
		if edit.Before == *edit.After {
			return "", fmt.Errorf("graph repair edit makes no change")
		}
		edits = append(edits, replacement{start, start + len(edit.Before), *edit.After})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	for i := 1; i < len(edits); i++ {
		if edits[i].start < edits[i-1].end {
			return "", fmt.Errorf("graph repair edits overlap")
		}
	}
	for i := len(edits) - 1; i >= 0; i-- {
		edit := edits[i]
		draft = draft[:edit.start] + edit.text + draft[edit.end:]
	}
	if strings.TrimSpace(draft) == "" {
		return "", fmt.Errorf("graph repair removes the entire report")
	}
	return draft, nil
}
