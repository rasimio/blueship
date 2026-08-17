package handler

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/rasimio/blueship/internal/core"
)

// A task's evidence ledger: what it has already established, carried from one
// iteration to the next.
//
// The hole it fills. An iteration is a fresh turn whose only inheritance is
// progress.Summary — the previous reply truncated to 500 chars — plus the
// plan. The bodies of every page the task read sit in agent_task_tool_outputs
// and are handed back to nobody. So an agent whose job is verifying its own
// claims has no way to know a claim is already verified, and re-derives it by
// re-reading the source.
//
// Production, task acc49aa2: the same self-audit — "CLAIM 1: unitree_sdk2 is
// BSD-3-Clause…" — reappeared at iterations 8, 9 and 10, and 102 of the run's
// 196 fetches were re-downloads. That is not a model looping stupidly; it is
// amnesia, and the fix is to let one iteration hand the next what it found.
//
// The model already produces this material in its scratchpad and throws it
// away. Here it is asked for it in a parseable block instead, and the handler
// owns the ledger: the model proposes entries, the handler dedupes, bounds and
// carries them.
const (
	// One claim's supporting quote. Long enough to be checkable, short enough
	// that a full ledger cannot crowd out the turn it is meant to inform.
	evidenceSpanMax  = 300
	evidenceClaimMax = 220
	// The ledger is a working set, not an archive. Past this many entries the
	// oldest fall off: a research task that has established forty facts is
	// writing its report, not still discovering.
	evidenceMaxEntries = 40
)

// evidenceJSONRE captures <<<EVIDENCE_JSON [ … ] >>>, following the same
// marker shape as the planner's PLAN_JSON block.
var evidenceJSONRE = regexp.MustCompile(`(?s)<<<EVIDENCE_JSON\s*(\[.*?\])\s*>>>`)

// EvidenceEntry is one established fact and where it came from.
type EvidenceEntry struct {
	Claim     string `json:"claim"`
	URL       string `json:"url"`
	Span      string `json:"span"`
	Iteration int    `json:"iteration"`
}

// parseEvidenceJSON pulls the ledger entries a reply proposes. A malformed or
// absent block is not an error: the ledger is an accelerator, and a task whose
// model never emits one behaves exactly as it did before.
func parseEvidenceJSON(reply string) []EvidenceEntry {
	m := evidenceJSONRE.FindStringSubmatch(reply)
	if m == nil {
		return nil
	}
	var entries []EvidenceEntry
	if err := json.Unmarshal([]byte(m[1]), &entries); err != nil {
		return nil
	}
	return entries
}

// stripEvidenceMarkers removes the ledger block from user-facing text. The
// ledger is machinery: a reader wants the report, not its bookkeeping.
func stripEvidenceMarkers(text string) string {
	return strings.TrimSpace(evidenceJSONRE.ReplaceAllString(text, ""))
}

// mergeEvidence folds one iteration's proposed entries into the carried
// ledger. The handler owns every invariant the model cannot be trusted with:
// an entry without a claim and a source is not evidence; a claim already
// established is not re-recorded (which is exactly what the model would do,
// since re-deriving is what this ledger exists to stop); and the whole thing
// stays bounded.
//
// A repeat of an existing claim refreshes nothing — the first recording keeps
// its iteration number, so the ledger reads as a history of when things were
// settled rather than of when they were last restated.
func mergeEvidence(carried, proposed []EvidenceEntry, iteration int) []EvidenceEntry {
	seen := make(map[string]bool, len(carried)+len(proposed))
	for _, e := range carried {
		seen[evidenceKey(e)] = true
	}
	out := carried
	for _, e := range proposed {
		e.Claim = strings.TrimSpace(e.Claim)
		e.URL = strings.TrimSpace(e.URL)
		e.Span = strings.TrimSpace(e.Span)
		// A claim with no source is the thing the grounding gate exists to
		// reject. It must not enter the ledger as though it were settled.
		if e.Claim == "" || core.NormalizeDocURL(e.URL) == "" {
			continue
		}
		if key := evidenceKey(e); seen[key] {
			continue
		} else {
			seen[key] = true
		}
		e.Claim = truncate(e.Claim, evidenceClaimMax)
		e.Span = truncate(e.Span, evidenceSpanMax)
		e.Iteration = iteration
		out = append(out, e)
	}
	if len(out) > evidenceMaxEntries {
		out = out[len(out)-evidenceMaxEntries:]
	}
	return out
}

// evidenceKey identifies a claim-source pair. URLs are normalized through the
// same function the fetch cache and the grounding auditor use, so one document
// does not acquire a second identity here.
func evidenceKey(e EvidenceEntry) string {
	return strings.ToLower(strings.Join(strings.Fields(e.Claim), " ")) +
		"\x00" + core.NormalizeDocURL(e.URL)
}

// formatEvidenceLedger renders what the task has already established, for the
// next iteration's turn. Explicitly framed as "do not re-verify": without that
// line the model reads it as context and re-checks it anyway, which is the
// behaviour this whole mechanism is paid to remove.
func formatEvidenceLedger(entries []EvidenceEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[established — already verified against sources in earlier iterations of THIS task]\n")
	b.WriteString("Treat these as settled. Do not refetch these sources to re-check them, and do not spend this iteration restating them; build on them or move to what is still unknown.\n")
	for i, e := range entries {
		fmt.Fprintf(&b, "%d. %s\n   source: %s (iteration %d)\n", i+1, e.Claim, e.URL, e.Iteration)
		if e.Span != "" {
			fmt.Fprintf(&b, "   span: %q\n", e.Span)
		}
	}
	b.WriteString("[/established]\n")
	return b.String()
}

// evidenceProtocol is the instruction that makes the ledger fill up. It lives
// in the framework rather than a host prompt file because the framework is
// what parses, dedupes and carries the block — a host that had to know the
// marker could silently stop emitting it and the ledger would just quietly
// stay empty.
const evidenceProtocol = "\n\n[evidence ledger]\n" +
	"End your reply with a block recording what you established THIS iteration from sources you actually read:\n" +
	"<<<EVIDENCE_JSON [{\"claim\":\"one factual assertion\",\"url\":\"source URL you fetched\",\"span\":\"short verbatim quote from that source\"}] >>>\n" +
	"Only facts supported by a span you have in front of you. Nothing you inferred, remembered, or intend to check later. " +
	"Omit the block entirely if this iteration established nothing new. " +
	"The block is machinery — it is stripped from the report, so do not reference it in your prose.\n" +
	"[/evidence ledger]"
