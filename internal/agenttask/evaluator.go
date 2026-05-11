package agenttask

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

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

// reURLAny extracts any http(s) URL-looking substring. Deliberately
// greedy on stop chars — we re-parse each match through url.Parse and
// trust the structured validation more than a precise regex.
var reURLAny = regexp.MustCompile(`https?://[^\s)\]\\"'<>]+`)

// extractURLs returns the set of distinct, syntactically-valid http(s)
// URLs found in `text`. "Syntactically valid" means url.Parse succeeds,
// scheme is http or https, host has a dot, and the path doesn't end in
// `..` or contain double-scheme artifacts ("httpshttps", "https://https://").
// These last filters catch the 2026-05-11 d2f6964c failure mode where
// the model emitted `httpshttps://ai.meta.com/...` and `way_ve.ai`
// — substring-counting passed the gate even though the URLs were dead.
// Hosts compared case-insensitively.
func extractURLs(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, raw := range reURLAny.FindAllString(text, -1) {
		raw = strings.TrimRight(raw, ".,;:!?")
		if !isLooksLikeRealURL(raw) {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		key := strings.ToLower(u.Host) + u.Path
		out[key] = struct{}{}
	}
	return out
}

// isLooksLikeRealURL filters out the most common synthesis artifacts
// before we even hand the string to url.Parse — saves a parse on
// obviously corrupted input.
func isLooksLikeRealURL(s string) bool {
	low := strings.ToLower(s)
	// Double-scheme artifact ("httpshttps://..."), double underscore in
	// host segment ("way_ve.ai"), trailing dot-dot ("/foo/.."), and
	// repeated scheme inside the URL all indicate model hallucination
	// rather than a fetched source.
	if strings.Contains(low, "httpshttps") || strings.Contains(low, "httphttp") {
		return false
	}
	if strings.HasSuffix(strings.TrimRight(s, "/"), "..") {
		return false
	}
	// host must have a dot and at least one char on each side
	hostStart := strings.Index(low, "://") + 3
	if hostStart < 3 {
		return false
	}
	rest := s[hostStart:]
	slash := strings.IndexAny(rest, "/?#")
	host := rest
	if slash >= 0 {
		host = rest[:slash]
	}
	if !strings.Contains(host, ".") {
		return false
	}
	// reject underscores in host (RFC 3986 forbids them; their presence
	// almost always means escape-leakage like `way_ve.ai`)
	if strings.Contains(host, "_") {
		return false
	}
	return true
}

// loadFetchedURLs pulls the set of URLs the agent_task actually invoked
// browser_fetch on, by reading the tool_calls jsonb column of every
// iteration row for this task. The set is normalised the same way
// extractURLs normalises (lowercase host + path) so intersection works.
//
// Returns an empty set on any error — caller treats "no fetched URLs"
// as a hard failure for evidentiary tasks (you can't cite what you
// didn't read), so a DB hiccup falls on the safe side rather than
// silently passing fake citations.
func loadFetchedURLs(ctx context.Context, deps core.AgentDeps, taskID uuid.UUID) map[string]struct{} {
	out := map[string]struct{}{}
	if deps.DB == nil {
		return out
	}
	db, err := deps.DB("ship")
	if err != nil {
		return out
	}
	// Pull every tool_call where name='browser_fetch' across all
	// iterations of this task. jsonb_path_query unwinds the array.
	rows, err := db.QueryContext(ctx, `
		SELECT tc->>'input' AS input
		FROM blueship.agent_task_iterations,
		     jsonb_array_elements(tool_calls) AS tc
		WHERE task_id = $1 AND tc->>'name' = 'browser_fetch'`, taskID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var input string
		if err := rows.Scan(&input); err != nil {
			continue
		}
		// tool input is JSON; the URL is in `.url`. Cheap shortcut: just
		// regex any http(s) substring out of the input — handles both
		// {"url":"..."} and any other shape the agent's input takes.
		for u := range extractURLs(input) {
			out[u] = struct{}{}
		}
	}
	return out
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
	// citations" phrase, the evaluator (a) counts syntactically-valid
	// URLs in the result, (b) cross-references them against URLs the
	// task actually invoked browser_fetch on, and (c) hard-fails before
	// the LLM call if (b) is empty or smaller than required. Keeps
	// blueship language-neutral; the persona layer (Arlene cortex) is
	// where "research" semantics live and where the criteria is shaped.
	requiredURLs := extractURLRequirement(*task.AcceptanceCriteria)
	resultURLs := extractURLs(result)
	urlCount := len(resultURLs)
	var verifiedURLCount int
	if requiredURLs > 0 {
		fetchedURLs := loadFetchedURLs(ctx, deps, task.ID)
		for u := range resultURLs {
			if _, ok := fetchedURLs[u]; ok {
				verifiedURLCount++
			}
		}
		// Hard gate 1: research task with searches but ZERO real fetches
		// — model is citing without reading. The 2026-05-11 d2f6964c
		// task did exactly this: 9 browser_search calls, 0 browser_fetch,
		// and a "references" section with `httpshttps://` corrupted URLs
		// that the substring-count gate accepted.
		if len(fetchedURLs) == 0 {
			deps.Logger.Info("acceptance evaluator: hard-fail (no fetched URLs)",
				"task_id", task.ID, "urls_in_result", urlCount)
			return AcceptanceVerdict{
				Met: false,
				Reason: fmt.Sprintf(
					"hard gate: result lists %d URL-like strings but browser_fetch was never called — citations are synthesised, not read. Open at least %d distinct pages via browser_fetch and cite them.",
					urlCount, requiredURLs),
			}
		}
		// Hard gate 2: too few URLs in the result intersect with what
		// was actually fetched. Substring count alone is too easy to
		// game with `httpshttps://` / `way_ve.ai` / similar fakes.
		if verifiedURLCount < requiredURLs {
			deps.Logger.Info("acceptance evaluator: hard-fail (verified URL count low)",
				"task_id", task.ID,
				"required", requiredURLs,
				"verified", verifiedURLCount,
				"urls_in_result", urlCount,
				"urls_fetched", len(fetchedURLs))
			return AcceptanceVerdict{
				Met: false,
				Reason: fmt.Sprintf(
					"hard gate: only %d of %d cited URLs were actually fetched (result has %d URL-like strings, %d distinct URLs were fetched). Fetched URLs must appear in the result, not synthesised ones.",
					verifiedURLCount, requiredURLs, urlCount, len(fetchedURLs)),
			}
		}
	}

	system := `You are a strict acceptance-criteria reviewer. Given a task description, an acceptance_criteria string, and a result, decide whether the result demonstrably meets every part of the criteria.

Reply with JSON only, no prose:
  {"met": true, "reason": "<one sentence why>"}
  {"met": false, "reason": "<one sentence naming what's missing>"}

Be strict: half-done work is not done. Criteria like "code is reviewed" require evidence the review happened, not just that code exists. If the criteria specifies a minimum number of URL citations / sources, the result must contain at least that many distinct source URLs — a polished write-up with zero or too-few URLs is a synthesis from training data, not evidence-grounded work.`

	extraHint := ""
	if requiredURLs > 0 {
		extraHint = fmt.Sprintf("\n\nEVIDENTIARY GATE (parsed from criteria): the criteria asks for at least %d URL citations. The hard pre-gate already verified that %d distinct URLs in the result match URLs the task actually fetched via browser_fetch — that part passed. Your job is to assess *quality*: do the citations actually support the claims made? Is the structure coherent? Use met=false if the report is technically cited but the cited pages don't back the specific assertions (e.g. cited a homepage instead of the relevant article).",
			requiredURLs, verifiedURLCount)
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
