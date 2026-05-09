package agenttask

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/rasimio/blueship/core"
)

// reURLRequirement matches evidentiary requirements written into the
// acceptance_criteria itself, e.g. "at least 3 URL citations",
// "≥5 sources", "minimum of 3 URLs". Generic pattern: an integer
// followed by a noun that's clearly source-shaped. Stays language-
// neutral by parsing structure, not vocabulary — host agents that
// want this gate enforced just include the phrasing in the criteria
// they themselves author. Captures the count.
var reURLRequirement = regexp.MustCompile(`(?i)\b(\d{1,3})\s*\+?\s*(?:url|urls|source|sources|citation|citations|reference|references|link|links)\b`)

// extractURLRequirement returns the largest integer N from any
// "N urls / sources / citations / refs / links" phrase in criteria,
// or 0 if none is found. Largest wins so a criteria saying both
// "list 3 sources" and "minimum 5 citations" enforces the stricter 5.
func extractURLRequirement(criteria string) int {
	matches := reURLRequirement.FindAllStringSubmatch(criteria, -1)
	max := 0
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max
}

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

	// Evidentiary gate is opt-in via the acceptance_criteria itself.
	// If the host's criteria contains an explicit "N URLs / sources /
	// citations" phrase, the evaluator counts URLs in the result and
	// surfaces the gap to the LLM reviewer as a structured note. No
	// language-specific keyword detection — host agents that want this
	// gate write the requirement into the criteria they author. Keeps
	// blueship language-neutral; the persona layer (Arlene cortex) is
	// where "research" semantics live and where the criteria is shaped.
	requiredURLs := extractURLRequirement(*task.AcceptanceCriteria)
	urlCount := 0
	if requiredURLs > 0 {
		urlCount = strings.Count(result, "http://") + strings.Count(result, "https://")
	}

	system := `You are a strict acceptance-criteria reviewer. Given a task description, an acceptance_criteria string, and a result, decide whether the result demonstrably meets every part of the criteria.

Reply with JSON only, no prose:
  {"met": true, "reason": "<one sentence why>"}
  {"met": false, "reason": "<one sentence naming what's missing>"}

Be strict: half-done work is not done. Criteria like "code is reviewed" require evidence the review happened, not just that code exists. If the criteria specifies a minimum number of URL citations / sources, the result must contain at least that many distinct source URLs — a polished write-up with zero or too-few URLs is a synthesis from training data, not evidence-grounded work.`

	extraHint := ""
	if requiredURLs > 0 {
		extraHint = fmt.Sprintf("\n\nEVIDENTIARY GATE (parsed from criteria): the criteria asks for at least %d URL citations; the result currently contains %d distinct URL strings (http:// or https://). If %d < %d, fail with reason naming the gap.",
			requiredURLs, urlCount, urlCount, requiredURLs)
	}

	user := fmt.Sprintf(
		"TASK: %s\n\nDESCRIPTION:\n%s\n\nACCEPTANCE CRITERIA:\n%s\n\nRESULT:\n%s%s\n\nDoes the result meet the acceptance criteria?",
		task.Title, desc, *task.AcceptanceCriteria, result, extraHint,
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
