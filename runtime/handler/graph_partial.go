package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rasimio/blueship/internal/core"
)

// graphPartialMaxTokens matches the finalizer: a partial report summarizes the
// same confirmed branches, and 4096 cut it off (production, 2026-09-23).
const graphPartialMaxTokens = 16384

// SynthesizeGraphPartial cannot research or mutate anything. Persisted confirmed
// results remain the deterministic fallback if this optional synthesis fails.
func (b *Background) SynthesizeGraphPartial(ctx context.Context, task core.AgentTask, steps []core.TaskStep, deps core.AgentDeps) (string, error) {
	if deps.Prompts == nil || deps.LLM == nil || deps.Config == nil {
		return "", fmt.Errorf("partial synthesis unavailable")
	}
	confirmed := map[string]string{}
	gaps := map[string]string{}
	drafts := map[string]any{}
	for _, step := range steps {
		if step.Status == "done" {
			confirmed[step.ID] = step.Goal + "\n" + step.Result
		} else {
			gaps[step.ID] = step.Goal + "\n" + step.LastError
			var cp graphStepCheckpoint
			if step.Kind == "read" && json.Unmarshal(step.Checkpoint, &cp) == nil {
				draft, review := cp.LastCandidate, cp.LastCandidateReview
				if cp.CandidateReady && strings.TrimSpace(cp.Output) != "" {
					draft, review = cp.Output, cp.ReviewReason
				}
				if strings.TrimSpace(draft) != "" {
					drafts[step.ID] = map[string]string{"goal": step.Goal, "draft": draft, "review_feedback": review}
				}
			}
		}
	}
	if len(confirmed) == 0 && len(drafts) == 0 {
		return "", nil
	}
	prompt, err := deps.Prompts.Get(ctx, "background-graph-partial")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("partial synthesis prompt missing")
	}
	model := deps.Config.Models.Primary
	if deps.ModelStore != nil {
		if ref := deps.ModelStore.Get(modelRoleForTask(task)); ref.Name != "" {
			model = ref
		}
	}
	input, err := json.Marshal(map[string]any{"current_datetime": b.graphCurrentDatetime(ctx, deps), "task": task.Title, "description": task.Description, "acceptance": task.AcceptanceCriteria, "confirmed_results": confirmed, "unfinished_steps": gaps, "unverified_drafts": drafts})
	if err != nil {
		return "", err
	}
	response, err := deps.LLM.Complete(ctx, core.CompletionRequest{Model: model.ForRouter(), System: prompt, Messages: []core.Message{{Role: "user", Content: core.NormalizeContent(string(input))}}, MaxTokens: graphPartialMaxTokens, Effort: model.Effort, ThinkingMode: model.ThinkingMode})
	if err != nil {
		return "", err
	}
	if response == nil || response.StopReason == "max_tokens" {
		return "", fmt.Errorf("empty partial synthesis response")
	}
	var body strings.Builder
	for _, block := range response.Content {
		if block.Type == "thinking" || block.Type == "redacted_thinking" {
			continue
		}
		if block.Type != "text" {
			return "", fmt.Errorf("partial synthesis requested a tool")
		}
		body.WriteString(block.Text)
	}
	return strings.TrimSpace(scratchpadRE.ReplaceAllString(body.String(), "")), nil
}
