package agenttask

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/rasimio/blueship/internal/core"
)

// Per-doc text cap fed into the grounding evaluator. Arxiv papers run
// 30-60K characters; capping below 25K cuts off section 4 content the
// auditor would flag as ungrounded even though the source does support
// the claim. groundingTotalBudget caps total context to fit safely in
// Sonnet 200K with the 8K output budget and prompt overhead.
//
// groundingMinWindow is the floor under one document's slice. Below it a
// document is a page header, and a header cannot support a claim — it can
// only fail one. Documents that would land under the floor are dropped
// from the audit set and declared, instead of being shown as stubs.
const (
	groundingPerDocCap     = 25_000
	groundingTotalBudget   = 250_000
	groundingMinWindow     = 4_000
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
// The auditor sees deduplicated documents, cited ones first, up to
// groundingPerDocCap chars each and groundingTotalBudget total; see
// selectGroundingDocs for why that ordering is load-bearing.
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

	modelRef := pickGroundingModel(deps)
	model := modelRef.ForRouter()
	if model == "" {
		return GroundingVerdict{Met: true, Reason: "no model configured for grounding eval"}
	}

	user, docStats := buildGroundingUserMessage(report, docs)
	deps.Logger.Info("grounding evaluator: audit set",
		"task_id", task.ID,
		"rows", docStats.Rows,
		"unique", docStats.Unique,
		"cited", docStats.Cited,
		"included", docStats.Included,
		"omitted", docStats.Omitted,
		"min_window", docStats.MinWindow,
	)

	resp, err := deps.LLM.Complete(ctx, core.CompletionRequest{
		Model:        model,
		System:       groundingSystemPrompt,
		Messages:     []core.Message{{Role: "user", Content: core.NormalizeContent(user)}},
		MaxTokens:    groundingMaxOutputToks,
		Temperature:  0.2,
		Effort:       modelRef.Effort,
		ThinkingMode: modelRef.ThinkingMode,
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
func pickGroundingModel(deps core.AgentDeps) core.ModelRef {
	if deps.ModelStore != nil {
		if ref := deps.ModelStore.Get("grounding_evaluator"); ref.Name != "" {
			return ref
		}
		if ref := deps.ModelStore.Get("compact"); ref.Name != "" {
			return ref
		}
		if ref := deps.ModelStore.Get("cortex"); ref.Name != "" {
			return ref
		}
	}
	if deps.Config != nil {
		return deps.Config.Models.Primary
	}
	return core.ModelRef{}
}

// groundingDoc is one document as the auditor will see it: the row, the
// URL it is addressed by, whether the report cites it, and how much of its
// text fits in the budget.
type groundingDoc struct {
	Doc    ToolOutput
	URL    string
	Title  string
	Cited  bool
	Window int // chars of Doc.Output the auditor gets
}

// groundingDocStats is what the selection did, for the log. A gate that
// silently shows the auditor less than it thinks reads as "the researcher
// made it up" on every claim it cannot see.
type groundingDocStats struct {
	Rows      int // tool-output rows loaded for the task
	Unique    int // distinct documents after dedup
	Cited     int // distinct documents the report links
	Included  int // documents that made it into the prompt
	Omitted   int // dropped because the budget ran out
	MinWindow int // smallest window any included doc got
}

// selectGroundingDocs turns every browser_fetch row a task ever recorded
// into the document set the auditor actually reads.
//
// Two rules, both learned from one production failure. A research task
// re-fetches the same handful of URLs every iteration, so by iteration 18
// the store held 205 rows for 96 distinct URLs — and the old code divided
// the budget across ROWS, handing the auditor 250000/205 = 1219 chars of
// each: the GitHub page header, twenty times over. It then reported six
// "hard ungrounded" claims whose supporting sentences sat at offsets 1398
// through 9579 of documents it had been given, failed the task on that
// basis, and burned the whole iteration budget doing it — each rejected
// iteration re-fetched the same pages, added rows, and shrank the window
// further.
//
// So: dedup by URL keeping the newest fetch, and spend the budget on the
// documents the report actually cites before anything else. A document
// that cannot get at least groundingMinWindow chars is dropped rather than
// included as a stub — a header-sized excerpt cannot support a claim, it
// can only fail one.
func selectGroundingDocs(report string, docs []ToolOutput) ([]groundingDoc, groundingDocStats) {
	stats := groundingDocStats{Rows: len(docs)}

	// Dedup by URL, newest wins: a later fetch of the same page is the
	// content the researcher worked from.
	latest := map[string]ToolOutput{}
	var order []string
	for _, d := range docs {
		url := groundingDocURL(d)
		if _, seen := latest[url]; !seen {
			order = append(order, url)
		}
		latest[url] = d
	}
	stats.Unique = len(order)

	cited := reportCitedURLs(report)
	var head, tail []groundingDoc // cited first, then the rest
	for _, url := range order {
		d := latest[url]
		g := groundingDoc{
			Doc:   d,
			URL:   url,
			Title: metaString(d.Metadata, "title"),
			// Match on both URLs the row carries: arxiv rewrites
			// /abs/ to /pdf/ at fetch time, and reports cite the
			// page they asked for.
			Cited: cited[normalizeDocURL(url)] ||
				cited[normalizeDocURL(metaString(d.Metadata, "requested_url"))],
		}
		if g.Cited {
			stats.Cited++
			head = append(head, g)
		} else {
			tail = append(tail, g)
		}
	}
	// Uncited documents are read newest-first: the last thing fetched is
	// the likeliest source of the newest paragraph.
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}

	budget := groundingTotalBudget
	var out []groundingDoc
	for _, g := range append(head, tail...) {
		if budget < groundingMinWindow {
			stats.Omitted++
			continue
		}
		g.Window = min(min(len(g.Doc.Output), groundingPerDocCap), budget)
		budget -= g.Window
		if stats.MinWindow == 0 || g.Window < stats.MinWindow {
			stats.MinWindow = g.Window
		}
		out = append(out, g)
	}
	stats.Included = len(out)
	return out, stats
}

// groundingDocURL addresses a fetched row. final_url is the document that
// was actually read (post-redirect, post abstract→PDF rewrite); the
// requested URL and the raw tool input are fallbacks for rows written
// before the metadata existed.
func groundingDocURL(d ToolOutput) string {
	if u := metaString(d.Metadata, "final_url"); u != "" {
		return u
	}
	if u := metaString(d.Metadata, "requested_url"); u != "" {
		return u
	}
	var in struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(d.ToolInput, &in) == nil && in.URL != "" {
		return in.URL
	}
	return ""
}

var reportURLRE = regexp.MustCompile(`https?://[^\s)\]}<>"'` + "`" + `]+`)

// reportCitedURLs is the set of documents the report points at, normalized.
func reportCitedURLs(report string) map[string]bool {
	cited := map[string]bool{}
	for _, raw := range reportURLRE.FindAllString(report, -1) {
		if u := normalizeDocURL(raw); u != "" {
			cited[u] = true
		}
	}
	return cited
}

// normalizeDocURL is core's normalizer, named locally for readability. The
// fetch cache matches a repeat request against stored rows with the same
// function on purpose: a document must not have one identity when the
// auditor budgets context for it and another when the tool decides whether
// it has already been read.
func normalizeDocURL(raw string) string { return core.NormalizeDocURL(raw) }

// buildGroundingUserMessage assembles the user prompt: report header, then
// each selected document with a "=== Doc N ===" separator. Omitted
// documents are declared rather than silently dropped — an auditor that
// believes it has everything reports absence as fabrication.
func buildGroundingUserMessage(report string, docs []ToolOutput) (string, groundingDocStats) {
	selected, stats := selectGroundingDocs(report, docs)

	var b strings.Builder
	b.WriteString("[report]\n")
	b.WriteString(report)
	b.WriteString("\n\n[fetched_documents]\n")
	for i, g := range selected {
		fmt.Fprintf(&b, "=== Doc %d: %s (%s)\n", i+1, g.Title, g.URL)
		text := g.Doc.Output
		if len(text) > g.Window {
			text = text[:g.Window] + "\n[...truncated...]"
		}
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	if stats.Omitted > 0 {
		fmt.Fprintf(&b, "[note] %d further fetched documents did not fit the audit budget and are not shown. "+
			"Judge only against what is above; do not treat a missing document as evidence of fabrication.\n",
			stats.Omitted)
	}
	return b.String(), stats
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
//   - support = grounded + 0.5 × partial
//   - ratio = support / total
//   - hardUngroundedTolerance = total / 20 (integer division)
//   - met = ratio >= 0.70 AND hardUngroundedCount <= tolerance
//
// Threshold 0.70 is the shadow-mode default; calibrated against real
// task data before enforcement flips on (see plan.md "Calibration
// window"). The hard-ungrounded tolerance is the relaxed form of the
// original "no hard ungrounded ever" rule: an S-tier research report
// of 20+ claims is allowed one imperfection (5% hard-ungrounded), while
// smaller reports (under 20 claims) must still be perfectly grounded
// because their absolute error budget is smaller. Calibrated from
// eval-smoke a0ad88ee (2026-05-14) where 19/20 claims passed but the
// 20th — a fabricated "long-horizon planning difficulties include
// autoregressive error accumulation" limitation — caused a binary
// reject of an otherwise S-quality report.
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

	var hardUngroundedCount int
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
				hardUngroundedCount++
				if firstHardClaim == nil {
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
	// Score with half-credit for partial claims. A "partial" verdict from
	// the auditor means "the fetched source supports this claim but not
	// verbatim / not in full" — that's load-bearing evidence, not a
	// hallucination. Counting partials as zero produced a perverse
	// outcome on the 2026-05-14 eval-smoke 441a1808 where the model
	// followed the attribution discipline perfectly (0 ungrounded across
	// 21 claims) but still failed because 8 of 21 were merely "partial"
	// — claims like *"V-JEPA matches generative methods on motion-heavy
	// tasks"* where the source supports the direction but not the exact
	// phrasing. Half-credit reflects that "partial" is between
	// "grounded" and "ungrounded" and rewards the conservative
	// rephrasing the attribution prompt asks for.
	support := float64(v.GroundedCount) + 0.5*float64(v.PartialCount)
	ratio := support / float64(v.TotalCount)
	hardUngroundedTolerance := v.TotalCount / 20
	v.Met = ratio >= groundedRatioThreshold && hardUngroundedCount <= hardUngroundedTolerance

	if v.Met {
		switch {
		case hardUngroundedCount == 0:
			v.Reason = fmt.Sprintf(
				"%d/%d claims grounded + %d partial (support %.0f%%), no hard-category ungrounded",
				v.GroundedCount, v.TotalCount, v.PartialCount, ratio*100)
		default:
			v.Reason = fmt.Sprintf(
				"%d/%d claims grounded + %d partial (support %.0f%%), %d hard ungrounded within tolerance of %d for %d-claim report",
				v.GroundedCount, v.TotalCount, v.PartialCount, ratio*100,
				hardUngroundedCount, hardUngroundedTolerance, v.TotalCount)
		}
		return v
	}

	// Reject reason: lead with the most damning detail. Hard category
	// ungrounded > ratio under threshold > everything else.
	var b strings.Builder
	fmt.Fprintf(&b, "%d/%d grounded + %d partial (support %.0f%%); ",
		v.GroundedCount, v.TotalCount, v.PartialCount, ratio*100)
	if hardUngroundedCount > hardUngroundedTolerance && firstHardClaim != nil {
		if hardUngroundedTolerance == 0 {
			fmt.Fprintf(&b, "ungrounded %s claim — %q",
				firstHardClaim.ClaimType, truncate(firstHardClaim.Claim, 140))
		} else {
			fmt.Fprintf(&b, "%d hard ungrounded claims, only %d tolerated for %d-claim report; first — %s claim %q",
				hardUngroundedCount, hardUngroundedTolerance, v.TotalCount,
				firstHardClaim.ClaimType, truncate(firstHardClaim.Claim, 140))
		}
		if firstHardClaim.Issue != "" {
			fmt.Fprintf(&b, " (issue: %s)", firstHardClaim.Issue)
		}
	} else {
		fmt.Fprintf(&b, "support ratio below %.0f%% threshold", groundedRatioThreshold*100)
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
