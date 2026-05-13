package agenttask

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/browser"
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
//
// Keys are produced via canonURLKey — preprint /abs/ and /pdf/ URLs
// collapse to the same key so the cross-reference gate matches across
// the abstract↔pdf rewrite that browser.Fetch performs internally.
func extractURLs(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, raw := range reURLAny.FindAllString(text, -1) {
		raw = strings.TrimRight(raw, ".,;:!?")
		if !isLooksLikeRealURL(raw) {
			continue
		}
		if key := canonURLKey(raw); key != "" {
			out[key] = struct{}{}
		}
	}
	return out
}

// canonURLKey produces the canonical map key for a URL: lowercase host
// plus path, with the abstract→PDF rewrite applied first so /abs/X and
// /pdf/X.pdf collapse to one key. Returns "" on any parse failure.
//
// The whole point of the canonicaliser is to make the cited-URL set
// (extracted from a report) symmetric with the fetched-URL set
// (recorded from browser.Fetch). Without it, a report citing
// `arxiv.org/abs/2401.12345` would fail the cross-reference gate
// because the fetched-doc row stores `arxiv.org/pdf/2401.12345.pdf`.
func canonURLKey(rawURL string) string {
	rewritten := browser.RewriteAbstractToPDF(rawURL)
	u, err := url.Parse(rewritten)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host) + u.Path
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
// browser_fetch on. Reads BOTH the iteration tool-call trace (the
// historical source, kept for tasks older than the tool_outputs
// migration) and the agent_task_tool_outputs persistence table (the
// authoritative source going forward — captures the requested URL the
// agent asked for AND the final URL after the abstract→PDF rewrite,
// both in the metadata jsonb).
// All URLs are canonicalised before insertion so the report-side and
// fetch-side sets intersect through the same key space.
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

	// Path 1: tool_calls jsonb. tool_calls.input is the URL the agent
	// asked for (pre-rewrite). Survives even when tool_outputs table is
	// missing (older tasks).
	rows, err := db.QueryContext(ctx, `
		SELECT tc->>'input' AS input
		FROM blueship.agent_task_iterations,
		     jsonb_array_elements(tool_calls) AS tc
		WHERE task_id = $1 AND tc->>'name' = 'browser_fetch'`, taskID)
	if err == nil {
		for rows.Next() {
			var input string
			if err := rows.Scan(&input); err != nil {
				continue
			}
			for u := range extractURLs(input) {
				out[u] = struct{}{}
			}
		}
		rows.Close()
	}

	// Path 2: agent_task_tool_outputs filtered to browser_fetch. Both
	// URL forms ride in metadata (requested_url + final_url) so the
	// rewrite from /abs/ to /pdf/ doesn't break the cross-reference.
	docRows, err := db.QueryContext(ctx, `
		SELECT metadata->>'requested_url', metadata->>'final_url'
		FROM blueship.agent_task_tool_outputs
		WHERE task_id = $1 AND tool_name = 'browser_fetch'`, taskID)
	if err == nil {
		defer docRows.Close()
		for docRows.Next() {
			var requested, final *string
			if err := docRows.Scan(&requested, &final); err != nil {
				continue
			}
			if requested != nil {
				if k := canonURLKey(*requested); k != "" {
					out[k] = struct{}{}
				}
			}
			if final != nil {
				if k := canonURLKey(*final); k != "" {
					out[k] = struct{}{}
				}
			}
		}
	}
	return out
}

// ToolOutput is one persisted tool-output row returned by
// loadToolOutputs. Generic enough to carry browser_fetch bodies, code
// reads, db query results, etc. — the consuming gate dereferences
// Metadata for whatever per-tool fields it needs.
type ToolOutput struct {
	ToolName     string
	ToolInput    json.RawMessage
	Output       string
	OutputFormat string         // "html" | "pdf" | "code" | "json" | "csv" | ...
	Metadata     map[string]any // parsed per-tool extras
	Iteration    int            // which iteration produced this
}

// loadToolOutputs returns every tool-output row recorded for a task,
// optionally filtered to a list of tool names. Ordered by created_at.
// Passing nil/empty toolNames returns every tool's outputs (useful for
// generic forensics views); passing []string{"browser_fetch"} narrows
// to research grounding's relevant slice.
//
// Returns nil on DB error; caller decides whether "no outputs" is a
// hard fail or a no-op.
func loadToolOutputs(ctx context.Context, deps core.AgentDeps, taskID uuid.UUID, toolNames []string) []ToolOutput {
	if deps.DB == nil {
		return nil
	}
	db, err := deps.DB("ship")
	if err != nil {
		return nil
	}
	var rows *sql.Rows
	if len(toolNames) == 0 {
		rows, err = db.QueryContext(ctx, `
			SELECT tool_name, tool_input::text, output, output_format,
			       COALESCE(metadata::text, '{}'), iteration
			FROM blueship.agent_task_tool_outputs
			WHERE task_id = $1
			ORDER BY created_at`, taskID)
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT tool_name, tool_input::text, output, output_format,
			       COALESCE(metadata::text, '{}'), iteration
			FROM blueship.agent_task_tool_outputs
			WHERE task_id = $1 AND tool_name = ANY($2)
			ORDER BY created_at`, taskID, pq.Array(toolNames))
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ToolOutput
	for rows.Next() {
		var (
			toolName, inputJSON, output, format, metaJSON string
			iter                                          int
		)
		if err := rows.Scan(&toolName, &inputJSON, &output, &format, &metaJSON, &iter); err != nil {
			continue
		}
		meta := map[string]any{}
		_ = json.Unmarshal([]byte(metaJSON), &meta)
		out = append(out, ToolOutput{
			ToolName:     toolName,
			ToolInput:    json.RawMessage(inputJSON),
			Output:       output,
			OutputFormat: format,
			Metadata:     meta,
			Iteration:    iter,
		})
	}
	return out
}

// metaString pulls a string field out of a parsed metadata map. Missing
// or non-string values return "" — callers use the empty result as a
// pre-condition signal (e.g. don't render a URL header for a doc with
// no final_url).
func metaString(meta map[string]any, key string) string {
	if v, ok := meta[key].(string); ok {
		return v
	}
	return ""
}

// AcceptanceVerdict is the parsed JSON the evaluator LLM returns,
// plus the optional Gate-C grounding sub-verdict captured in the
// same return value so the scheduler can persist both in one audit
// row.
type AcceptanceVerdict struct {
	Met    bool   `json:"met"`
	Reason string `json:"reason"`

	// Grounding carries the per-claim audit from Gate C. Nil when
	// Gate C didn't run this iteration (no criteria, no fetched docs,
	// LLM error, malformed verdict). Always populated when the
	// evaluator made the LLM call regardless of Met — the scheduler
	// records the verdict in agent_task_iterations.grounding_verdict
	// for forensics on both pass and reject paths.
	Grounding *GroundingVerdict `json:"grounding,omitempty"`
}

// ClaimGrounding is the auditor's verdict on one claim from the report.
//
// ClaimType classifies WHAT kind of assertion the claim makes — used
// both for "is this a hard category we won't tolerate ungrounded" and
// for the human-readable reject reason. Without a typed classification
// we'd be regex-matching claim text (which mis-flags "JEPA" or "Sora"
// as person names because they're capitalised).
//
// Status is grounded/partial/ungrounded. Partial is the common pattern
// where the document mentions the topic but doesn't support the
// specific attribution/structure asserted: claim says "DP-TA by Zhang
// et al.", doc only names "Xiong" — partial, claim_type=attribution.
type ClaimGrounding struct {
	Claim            string `json:"claim"`
	ClaimType        string `json:"claim_type"`
	Status           string `json:"status"`
	SupportingDocURL string `json:"supporting_doc_url,omitempty"`
	SupportingSpan   string `json:"supporting_span,omitempty"`
	Issue            string `json:"issue,omitempty"`
}

// GroundingVerdict is the structured output of Gate C: every claim
// classified by source-grounding, plus aggregate counts and a list of
// URLs the cortex MUST re-fetch before re-submitting if Met is false.
//
// Met combines two conditions: a numerical floor (grounded_ratio >=
// threshold) AND a hard-category gate (no attribution/architectural/
// numerical/quote claim is fully ungrounded). Either being violated
// is a reject — a report with 8/10 grounded but one ungrounded
// "X et al." attribution is still a hallucination, ratio be damned.
//
// RecheckURLs is the recheck-loop hand-off. When the cortex tries
// again, the next iteration's evaluator MUST verify these URLs were
// re-fetched in-iteration (Gate B'). Without that guard a model would
// just edit the wrong author's name out of the report on retry; the
// recheck rule forces it to re-read the page that's the source of
// truth before submitting again.
type GroundingVerdict struct {
	Met             bool             `json:"met"`
	Reason          string           `json:"reason"`
	Claims          []ClaimGrounding `json:"claims"`
	TotalCount      int              `json:"total_count"`
	GroundedCount   int              `json:"grounded_count"`
	PartialCount    int              `json:"partial_count"`
	UngroundedCount int              `json:"ungrounded_count"`
	RecheckURLs     []string         `json:"recheck_urls,omitempty"`
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

	// Gate C: claim-level source-grounding audit. SHADOW MODE — verdict
	// is computed and persisted into agent_task_iterations.grounding_
	// verdict for calibration, but the Met decision here remains
	// driven by the existing hard gates + qualitative LLM evaluator
	// below. Once we have ~20-30 tasks' worth of shadow data the
	// threshold gets pinned (likely 0.60-0.75 grounded_ratio) and Gate C
	// flips to enforce.
	var groundingVerdict *GroundingVerdict
	if requiredURLs > 0 {
		docs := loadToolOutputs(ctx, deps, task.ID, []string{"browser_fetch"})
		if len(docs) > 0 {
			v := evaluateGrounding(ctx, deps, task, result, docs)
			groundingVerdict = &v
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
		return AcceptanceVerdict{Met: true, Grounding: groundingVerdict}
	}

	resp, err := deps.LLM.Complete(ctx, core.CompletionRequest{
		Model:     model,
		System:    system,
		Messages:  []core.Message{{Role: "user", Content: core.NormalizeContent(user)}},
		MaxTokens: 256,
	})
	if err != nil {
		deps.Logger.Warn("acceptance evaluator: llm call failed", "task_id", task.ID, "error", err)
		return AcceptanceVerdict{Met: true, Grounding: groundingVerdict}
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
		return AcceptanceVerdict{Met: true, Grounding: groundingVerdict}
	}
	v.Grounding = groundingVerdict
	deps.Logger.Info("acceptance evaluator: verdict",
		"task_id", task.ID, "met", v.Met, "reason", v.Reason,
		"grounding_present", groundingVerdict != nil)
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

// Per-doc text cap fed into the grounding evaluator. Arxiv papers run
// 30-60K characters; capping below 25K cuts off section 4 content the
// auditor would flag as ungrounded even though the source does support
// the claim. groundingTotalBudget caps total context to fit safely in
// Sonnet 200K with the 8K output budget and prompt overhead.
const (
	groundingPerDocCap     = 25_000
	groundingTotalBudget   = 250_000
	groundingMaxOutputToks = 8192
)

// groundingSystemPrompt is the audit instructions. Long-form by design
// — Anthropic auto-caches system prompts >= 1024 tokens, so the cost is
// amortised across iterations. The taxonomy of claim_type values is the
// load-bearing part: without it the auditor would have to invent a
// classification on the fly and the verdict logic couldn't tell hard
// hallucinations from soft framing claims.
const groundingSystemPrompt = `You are a citation auditor for research reports.

You receive a research report and the full text of every document the researcher actually fetched. Your job: for each factual claim in the report, identify whether it is supported by a verbatim or near-verbatim span in one of the provided documents.

Output strict JSON only, no prose. Schema:
{
  "claims": [
    {
      "claim": "<verbatim excerpt from report — one factual assertion>",
      "claim_type": "attribution" | "architectural" | "numerical" | "quote" | "framing",
      "status": "grounded" | "partial" | "ungrounded",
      "supporting_doc_url": "<URL of supporting doc, or empty>",
      "supporting_span": "<verbatim quote <= 200 chars from that doc, or empty>",
      "issue": "<one-sentence explanation when status != grounded>"
    }
  ]
}

claim_type values (every claim MUST be classified):
- attribution: who did this work, where, when. "X et al. proposed Y at University Z in 2024". The strictest category — these are the most common fabrication target.
- architectural: structural facts. "Three-stage perception → modeling → decision pipeline", "Two-layer Neural+Symbolic", "RSSM uses deterministic + stochastic split".
- numerical: counts, scores, dates, dimensions. "200K context", "90.2% improvement", "trained on 10B tokens".
- quote: direct verbatim quotes from the doc.
- framing: high-level positioning. "The field is moving toward X", "This is the dominant paradigm". Hardest to ground; be lenient — only flag framing as ungrounded if the report asserts it as established consensus and no doc supports that framing.

Status rules:
- grounded: the document directly states this claim, with trivial rewording at most.
- partial: the document mentions the topic but doesn't support the specific assertion. Common pattern: claim attributes the work to "Zhang et al." but the doc only names "Xiong" — the framework is grounded but the attribution isn't (status=partial, claim_type=attribution).
- ungrounded: no document supports the claim at all. Usually means the researcher synthesised from prior knowledge.

Be strict on attribution, architectural, numerical, quote. Be lenient on framing (framing-ungrounded is a warning, not a failure).

Skip these — do NOT emit a claim entry:
- Transitions ("First...", "Importantly...", "In summary...")
- Executive-summary paraphrases of claims already classified later in the report
- Trivially-true statements ("Machine learning is a field of computer science")

Aim for 8-20 claim entries on a typical research report. Each entry should be one self-contained factual assertion the reader could verify by looking at the supporting span.`

// evaluateGrounding runs Gate C: per-claim source-grounding audit. Loads
// the fetched-doc bodies for the task, ships them to a separate
// auditor model with the report, parses the per-claim verdict, and
// computes a pass/fail decision based on grounded-ratio + a hard-
// category check.
//
// Never blocks on failure: any LLM/JSON/DB hiccup returns
// {Met: true, Reason: "<diagnostic>"} so a flaky evaluator doesn't
// turn into a denial-of-service against the cortex. The shadow-mode
// rollout (Deploy 1) records every verdict regardless of Met so we
// can calibrate the threshold from real data before flipping to
// enforcement.
//
// The auditor sees up to groundingPerDocCap chars per doc and
// groundingTotalBudget chars total; older docs get trimmed first
// when the cap binds.
func evaluateGrounding(ctx context.Context, deps core.AgentDeps, task core.AgentTask, report string, docs []ToolOutput) GroundingVerdict {
	if len(docs) == 0 {
		return GroundingVerdict{
			Met:    true,
			Reason: "no fetched documents to audit against (Gate A should have caught this)",
		}
	}
	if deps.LLM == nil {
		return GroundingVerdict{Met: true, Reason: "no LLM configured for grounding eval"}
	}

	model := pickGroundingModel(deps)
	if model == "" {
		return GroundingVerdict{Met: true, Reason: "no model configured for grounding eval"}
	}

	user := buildGroundingUserMessage(report, docs)

	resp, err := deps.LLM.Complete(ctx, core.CompletionRequest{
		Model:       model,
		System:      groundingSystemPrompt,
		Messages:    []core.Message{{Role: "user", Content: core.NormalizeContent(user)}},
		MaxTokens:   groundingMaxOutputToks,
		Temperature: 0.2,
	})
	if err != nil {
		deps.Logger.Warn("grounding evaluator: llm call failed",
			"task_id", task.ID, "model", model, "error", err)
		return GroundingVerdict{Met: true, Reason: "grounding LLM call failed: " + err.Error()}
	}

	raw := contentToText(resp.Content)
	verdict, parseErr := parseGroundingResponse(raw)
	if parseErr != nil {
		// Persist the diagnostic but don't block — malformed JSON is a
		// prompt-quality issue we'll fix offline once we see it.
		deps.Logger.Warn("grounding evaluator: parse failed",
			"task_id", task.ID, "error", parseErr, "raw_head", headForLog(raw))
		return GroundingVerdict{Met: true, Reason: "grounding evaluator JSON parse failed: " + parseErr.Error()}
	}

	verdict = scoreGroundingVerdict(verdict)
	deps.Logger.Info("grounding evaluator: verdict",
		"task_id", task.ID,
		"met", verdict.Met,
		"grounded", verdict.GroundedCount,
		"partial", verdict.PartialCount,
		"ungrounded", verdict.UngroundedCount,
		"total", verdict.TotalCount,
		"recheck_count", len(verdict.RecheckURLs),
	)
	return verdict
}

// pickGroundingModel resolves the auditor model with a fallback chain.
// Production should always have a row at role='grounding_evaluator';
// fallbacks exist so the gate degrades gracefully on a misconfigured
// dev install rather than refusing every task.
func pickGroundingModel(deps core.AgentDeps) string {
	if deps.ModelStore != nil {
		if m := deps.ModelStore.ForRouter("grounding_evaluator"); m != "" {
			return m
		}
		if m := deps.ModelStore.ForRouter("compact"); m != "" {
			return m
		}
		if m := deps.ModelStore.ForRouter("cortex"); m != "" {
			return m
		}
	}
	if deps.Config != nil {
		return deps.Config.Models.Primary.ForRouter()
	}
	return ""
}

// buildGroundingUserMessage assembles the user prompt: report header,
// then every fetched doc with a "=== Doc N ===" separator. Docs are
// truncated to groundingPerDocCap chars; if their combined size exceeds
// groundingTotalBudget we trim from the END of the list (oldest fetches
// first) — recency is a decent priority signal when we can't fit
// everything. A future Phase C TODO swaps this for per-claim retrieval.
func buildGroundingUserMessage(report string, docs []ToolOutput) string {
	// Per-doc cap. Adaptive: if the natural total at 25K/doc would
	// exceed budget, shrink the per-doc cap so all docs fit.
	perDoc := groundingPerDocCap
	if got := perDoc * len(docs); got > groundingTotalBudget && len(docs) > 0 {
		perDoc = groundingTotalBudget / len(docs)
	}

	var b strings.Builder
	b.WriteString("[report]\n")
	b.WriteString(report)
	b.WriteString("\n\n[fetched_documents]\n")
	for i, d := range docs {
		title := metaString(d.Metadata, "title")
		// Prefer final_url (post-rewrite, real PDF) but fall back to
		// requested_url if the tool didn't record both.
		docURL := metaString(d.Metadata, "final_url")
		if docURL == "" {
			docURL = metaString(d.Metadata, "requested_url")
		}
		fmt.Fprintf(&b, "=== Doc %d: %s (%s)\n", i+1, title, docURL)
		text := d.Output
		if len(text) > perDoc {
			text = text[:perDoc] + "\n[...truncated...]"
		}
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	return b.String()
}

// parseGroundingResponse strips any leading/trailing prose, finds the
// outer JSON object, and unmarshals into a GroundingVerdict's Claims
// field. The scoring step fills in totals + Met + Reason after parse.
func parseGroundingResponse(raw string) (GroundingVerdict, error) {
	body := strings.TrimSpace(raw)
	start := strings.Index(body, "{")
	end := strings.LastIndex(body, "}")
	if start < 0 || end <= start {
		return GroundingVerdict{}, fmt.Errorf("no JSON object found in response")
	}
	body = body[start : end+1]

	var parsed struct {
		Claims []ClaimGrounding `json:"claims"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return GroundingVerdict{}, err
	}
	return GroundingVerdict{Claims: parsed.Claims}, nil
}

// scoreGroundingVerdict fills in totals, Met decision, Reason, and the
// RecheckURLs hand-off. Verdict.Claims must already be populated.
//
// Met logic:
//   - grounded_ratio = grounded / total
//   - hasHardUngrounded = any ungrounded claim with hard claim_type
//   - met = ratio >= 0.70 AND !hasHardUngrounded
//
// Threshold 0.70 is the shadow-mode default; calibrated against real
// task data before enforcement flips on (see plan.md "Calibration
// window"). hasHardUngrounded is the load-bearing guard against the
// "8/10 grounded but one wrong attribution" pattern.
//
// No total-count floor: "report must have >= 5 claims" creates the
// inverted incentive to inflate claim count. A tight 3-claim report
// that's 3/3 grounded is strictly better than a 12-claim report that's
// 9/12 grounded with 1 fabricated attribution.
func scoreGroundingVerdict(v GroundingVerdict) GroundingVerdict {
	const groundedRatioThreshold = 0.70

	hardCategories := map[string]bool{
		"attribution":   true,
		"architectural": true,
		"numerical":     true,
		"quote":         true,
	}

	var hasHardUngrounded bool
	var firstHardClaim *ClaimGrounding
	var recheck []string
	seenRecheck := map[string]struct{}{}

	for i := range v.Claims {
		c := &v.Claims[i]
		switch c.Status {
		case "grounded":
			v.GroundedCount++
		case "partial":
			v.PartialCount++
		case "ungrounded":
			v.UngroundedCount++
			if hardCategories[c.ClaimType] {
				if !hasHardUngrounded {
					hasHardUngrounded = true
					firstHardClaim = c
				}
				if c.ClaimType == "attribution" || c.ClaimType == "architectural" {
					if u := strings.TrimSpace(c.SupportingDocURL); u != "" {
						if _, dup := seenRecheck[u]; !dup {
							seenRecheck[u] = struct{}{}
							recheck = append(recheck, u)
						}
					}
				}
			}
		}
	}
	v.TotalCount = v.GroundedCount + v.PartialCount + v.UngroundedCount
	v.RecheckURLs = recheck

	if v.TotalCount == 0 {
		v.Met = true
		v.Reason = "auditor returned no classifiable claims; nothing to verify"
		return v
	}
	ratio := float64(v.GroundedCount) / float64(v.TotalCount)
	v.Met = ratio >= groundedRatioThreshold && !hasHardUngrounded

	if v.Met {
		v.Reason = fmt.Sprintf(
			"%d/%d claims grounded (%.0f%%), no hard-category ungrounded",
			v.GroundedCount, v.TotalCount, ratio*100)
		return v
	}

	// Reject reason: lead with the most damning detail. Hard category
	// ungrounded > ratio under threshold > everything else.
	var b strings.Builder
	fmt.Fprintf(&b, "%d/%d claims grounded (%.0f%%); ",
		v.GroundedCount, v.TotalCount, ratio*100)
	if hasHardUngrounded && firstHardClaim != nil {
		fmt.Fprintf(&b, "ungrounded %s claim — %q",
			firstHardClaim.ClaimType, truncate(firstHardClaim.Claim, 140))
		if firstHardClaim.Issue != "" {
			fmt.Fprintf(&b, " (issue: %s)", firstHardClaim.Issue)
		}
	} else {
		fmt.Fprintf(&b, "grounded ratio below %.0f%% threshold", groundedRatioThreshold*100)
	}
	if len(v.RecheckURLs) > 0 {
		fmt.Fprintf(&b, "; you MUST re-fetch and re-verify: %s",
			strings.Join(v.RecheckURLs, ", "))
	}
	v.Reason = b.String()
	return v
}

// headForLog returns a single-line preview suitable for log lines.
func headForLog(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "…"
}

// truncate is a rune-safe one-liner for embedded log/error strings.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
