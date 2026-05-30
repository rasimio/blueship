package agenttask

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rasimio/blueship/internal/core"
)

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
