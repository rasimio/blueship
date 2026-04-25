package agenttask

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rasimio/blueship/core"
)

// AcceptanceVerdict is the parsed JSON the evaluator LLM returns.
type AcceptanceVerdict struct {
	Met    bool   `json:"met"`
	Reason string `json:"reason"`
}

// evaluateAcceptance asks the configured LLM whether a task result
// satisfies the task's acceptance_criteria. Tasks without criteria
// always pass. The evaluator returns Met=true on any error so a
// transient LLM failure does not block a handler-claimed completion;
// the failure is logged for the operator to review.
//
// Prompt is intentionally tight: ask for JSON, parse it, treat anything
// non-parseable as a met=true fallback. The agent_task scheduler is
// not the right place to argue with the LLM about format.
func evaluateAcceptance(ctx context.Context, deps core.AgentDeps, task core.AgentTask, result string) AcceptanceVerdict {
	if task.AcceptanceCriteria == nil || strings.TrimSpace(*task.AcceptanceCriteria) == "" {
		return AcceptanceVerdict{Met: true}
	}
	if deps.LLM == nil {
		return AcceptanceVerdict{Met: true}
	}

	desc := ""
	if task.Description != nil {
		desc = *task.Description
	}

	system := `You are a strict acceptance-criteria reviewer. Given a task description, an acceptance_criteria string, and a result, decide whether the result demonstrably meets every part of the criteria.

Reply with JSON only, no prose:
  {"met": true, "reason": "<one sentence why>"}
  {"met": false, "reason": "<one sentence naming what's missing>"}

Be strict: half-done work is not done. Criteria like "code is reviewed" require evidence the review happened, not just that code exists.`

	user := fmt.Sprintf(
		"TASK: %s\n\nDESCRIPTION:\n%s\n\nACCEPTANCE CRITERIA:\n%s\n\nRESULT:\n%s\n\nDoes the result meet the acceptance criteria?",
		task.Title, desc, *task.AcceptanceCriteria, result,
	)

	model := deps.Config.Models.Primary.ForRouter()
	if model == "" {
		return AcceptanceVerdict{Met: true}
	}

	resp, err := deps.LLM.Complete(ctx, core.CompletionRequest{
		Model:     model,
		System:    system,
		Messages:  []core.Message{{Role: "user", Content: core.NormalizeContent(user)}},
		MaxTokens: 256,
	})
	if err != nil {
		deps.Logger.Warn("acceptance evaluator: llm call failed", "task_id", task.ID, "error", err)
		return AcceptanceVerdict{Met: true}
	}

	raw := contentToText(resp.Content)
	body := strings.TrimSpace(raw)
	if start := strings.Index(body, "{"); start >= 0 {
		if end := strings.LastIndex(body, "}"); end > start {
			body = body[start : end+1]
		}
	}

	var v AcceptanceVerdict
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		deps.Logger.Warn("acceptance evaluator: malformed verdict",
			"task_id", task.ID, "raw", raw)
		return AcceptanceVerdict{Met: true}
	}
	deps.Logger.Info("acceptance evaluator: verdict",
		"task_id", task.ID, "met", v.Met, "reason", v.Reason)
	return v
}

func contentToText(blocks []core.ContentBlock) string {
	var out strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			out.WriteString(b.Text)
		}
	}
	return out.String()
}

// injectFeedback merges the reviewer's reason into a progress JSON blob
// under the "acceptance_feedback" key so the next iteration sees what
// the previous result was flagged for.
func injectFeedback(progress json.RawMessage, reason string) json.RawMessage {
	if reason == "" {
		return progress
	}
	if len(progress) == 0 {
		progress = json.RawMessage(`{}`)
	}
	var m map[string]any
	if err := json.Unmarshal(progress, &m); err != nil {
		// Couldn't merge; preserve the original blob.
		return progress
	}
	if m == nil {
		m = map[string]any{}
	}
	m["acceptance_feedback"] = reason
	out, err := json.Marshal(m)
	if err != nil {
		return progress
	}
	return out
}
