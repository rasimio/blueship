package handler

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEvidenceLedgerCarriesFindingsAcrossIterations(t *testing.T) {
	reply := `Checked the SDK docs.

<<<EVIDENCE_JSON [
  {"claim":"unitree_sdk2 is BSD-3-Clause","url":"https://github.com/unitreerobotics/unitree_sdk2","span":"BSD 3-Clause License"},
  {"claim":"state arrives on lf/sportmodestate","url":"https://github.com/unitreerobotics/unitree_ros2","span":"subscribing \"lf/sportmodestate\" topic"}
] >>>`

	proposed := parseEvidenceJSON(reply)
	if len(proposed) != 2 {
		t.Fatalf("parsed %d entries, want 2", len(proposed))
	}
	ledger := mergeEvidence(nil, proposed, 3)
	if len(ledger) != 2 {
		t.Fatalf("ledger has %d entries, want 2", len(ledger))
	}
	if ledger[0].Iteration != 3 {
		t.Errorf("entry iteration = %d, want the iteration that established it (3)", ledger[0].Iteration)
	}

	// The block is machinery and must not survive into the report.
	if strings.Contains(stripEvidenceMarkers(reply), "EVIDENCE_JSON") {
		t.Error("the ledger block leaked into user-facing text")
	}

	// And the next iteration has to be told it is settled, not merely shown it
	// — the amnesia this exists to fix is precisely a model re-verifying what
	// it already knows.
	block := formatEvidenceLedger(ledger)
	for _, want := range []string{
		"unitree_sdk2 is BSD-3-Clause",
		"https://github.com/unitreerobotics/unitree_ros2",
		"Do not refetch these sources to re-check them",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("rendered ledger is missing %q", want)
		}
	}
}

// The model's own habit is to restate what it established last iteration —
// that is the loop production actually ran, with the same "CLAIM 1:
// unitree_sdk2 is BSD-3-Clause…" self-audit reappearing at iterations 8, 9
// and 10. A ledger that grew a row each time would carry the amnesia forward
// instead of curing it.
func TestEvidenceLedgerDoesNotGrowOnRestatement(t *testing.T) {
	first := mergeEvidence(nil, []EvidenceEntry{
		{Claim: "unitree_sdk2 is BSD-3-Clause", URL: "https://github.com/unitreerobotics/unitree_sdk2", Span: "BSD 3-Clause"},
	}, 4)
	// Same claim, restated with cosmetic differences a model would produce:
	// wrapped whitespace, a trailing slash, http instead of https.
	second := mergeEvidence(first, []EvidenceEntry{
		{Claim: "unitree_sdk2   is BSD-3-Clause", URL: "http://github.com/unitreerobotics/unitree_sdk2/", Span: "BSD 3-Clause License"},
	}, 5)
	if len(second) != 1 {
		t.Fatalf("restating a settled claim added a row: %#v", second)
	}
	if second[0].Iteration != 4 {
		t.Errorf("entry iteration = %d, want the first recording (4) — the ledger records when a fact was settled", second[0].Iteration)
	}
}

func TestEvidenceLedgerRejectsUnsourcedAndStaysBounded(t *testing.T) {
	unsourced := mergeEvidence(nil, []EvidenceEntry{
		{Claim: "the robot supports ROS2", URL: "", Span: "from memory"},
		{Claim: "", URL: "https://example.test/doc", Span: "a span with no claim"},
	}, 1)
	if len(unsourced) != 0 {
		t.Fatalf("unsourced claims entered the ledger: %#v", unsourced)
	}

	var many []EvidenceEntry
	for i := range evidenceMaxEntries * 2 {
		many = append(many, EvidenceEntry{
			Claim: fmt.Sprintf("claim %d", i),
			URL:   fmt.Sprintf("https://example.test/%d", i),
			Span:  strings.Repeat("s", evidenceSpanMax*2),
		})
	}
	ledger := mergeEvidence(nil, many, 2)
	if len(ledger) != evidenceMaxEntries {
		t.Fatalf("ledger holds %d entries, want it capped at %d", len(ledger), evidenceMaxEntries)
	}
	// The cap keeps the newest: a task deep in its run needs what it just
	// found, not what it found first.
	if !strings.Contains(ledger[len(ledger)-1].Claim, fmt.Sprint(evidenceMaxEntries*2-1)) {
		t.Errorf("cap dropped the newest entry instead of the oldest: last = %q", ledger[len(ledger)-1].Claim)
	}
	// Spans are cut to the cap plus truncate's "..." marker.
	for _, e := range ledger {
		if n := utf8.RuneCountInString(e.Span); n > evidenceSpanMax+3 {
			t.Fatalf("span kept at %d runes, want <= %d", n, evidenceSpanMax+3)
		}
	}
}

// A task with no ledger must render nothing at all — an empty "[established]"
// header would tell the model it has established nothing, which is a claim,
// not an absence.
func TestEvidenceLedgerIsSilentWhenEmpty(t *testing.T) {
	if got := formatEvidenceLedger(nil); got != "" {
		t.Errorf("empty ledger rendered %q", got)
	}
	if got := parseEvidenceJSON("a reply with no block at all"); got != nil {
		t.Errorf("parsed %#v out of a reply with no block", got)
	}
	if got := parseEvidenceJSON("<<<EVIDENCE_JSON [not json] >>>"); got != nil {
		t.Errorf("malformed block parsed as %#v; it must degrade to no ledger", got)
	}
}

// The ledger rides in progress, which is what the scheduler persists between
// iterations — if it did not round-trip through that JSON it would be lost
// exactly where the amnesia starts.
func TestEvidenceSurvivesProgressRoundTrip(t *testing.T) {
	p := bgProgress{Evidence: mergeEvidence(nil, []EvidenceEntry{
		{Claim: "c", URL: "https://example.test/d", Span: "s"},
	}, 7)}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back bgProgress
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Evidence) != 1 || back.Evidence[0].Claim != "c" || back.Evidence[0].Iteration != 7 {
		t.Fatalf("ledger did not survive progress: %#v", back.Evidence)
	}
}
