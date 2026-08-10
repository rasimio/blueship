package gateway

import (
	"strings"
	"testing"
)

// The dialogue window starts at the last soft summary, so this text is the only
// thing carrying what happened before that point. Order is the whole content of
// the function: hard compaction summarises and deletes, a soft summary is
// appended later and covers newer ground. Reversed, the model reads the recent
// past as if it preceded the distant past — worse than having neither, because a
// wrong order is asserted with exactly the same confidence as a right one.
func TestComposeSummariesPutsTheOlderOneFirst(t *testing.T) {
	got := composeSummaries("КОМПАКТ: самое давнее", "СВОДКА: то, что было позже")

	hard := strings.Index(got, "КОМПАКТ")
	soft := strings.Index(got, "СВОДКА")
	switch {
	case hard < 0 || soft < 0:
		t.Fatalf("one of the summaries was dropped: %q", got)
	case hard > soft:
		t.Errorf("the newer summary comes first, so the model reads the conversation backwards: %q", got)
	}
	if !strings.Contains(got, "\n\n") {
		t.Errorf("the two summaries run together with no separation: %q", got)
	}
}

func TestComposeSummariesHandlesEitherBeingAbsent(t *testing.T) {
	if got := composeSummaries("", "только сводка"); got != "только сводка" {
		t.Errorf("soft alone = %q, want it unchanged and unpadded", got)
	}
	if got := composeSummaries("только компакт", ""); got != "только компакт" {
		t.Errorf("hard alone = %q, want it unchanged and unpadded", got)
	}
	if got := composeSummaries("", ""); got != "" {
		t.Errorf("neither = %q, want empty — an empty summary header in the prompt is a lie about there being context", got)
	}
	// Whitespace-only must count as absent, or the prompt gains a summary header
	// wrapped around nothing.
	if got := composeSummaries("   \n ", "\t"); got != "" {
		t.Errorf("whitespace-only summaries = %q, want empty", got)
	}
}
