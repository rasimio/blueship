package gateway

import (
	"strings"
	"testing"
)

// The failure text must not read as an empty memory. A model told "no
// match" will happily conclude the event never happened; a model told the
// lookup did not run must not.
func TestUnavailableGuidanceDoesNotClaimAnEmptyMemory(t *testing.T) {
	var b strings.Builder
	appendMemoryUnavailableGuidance(&b)

	got := b.String()
	if !strings.Contains(got, "[memory unavailable]") {
		t.Fatalf("missing the block marker:\n%s", got)
	}
	for _, must := range []string{
		"did not run to completion",
		"NOT the same as finding nothing",
	} {
		if !strings.Contains(got, must) {
			t.Fatalf("guidance lost the distinction it exists for (%q):\n%s", must, got)
		}
	}
	// The no-match wording would invite exactly the wrong conclusion.
	if strings.Contains(got, "No user-specific Memory/Association item matched") {
		t.Fatal("failure guidance reused the no-match text — the two states are collapsed again")
	}
}

func TestUnavailableGuidanceAppendsAfterExistingContent(t *testing.T) {
	var b strings.Builder
	b.WriteString("[active rules]\nsomething\n[/active rules]")
	appendMemoryUnavailableGuidance(&b)

	got := b.String()
	if !strings.HasPrefix(got, "[active rules]") {
		t.Fatal("existing guidance was clobbered")
	}
	if !strings.Contains(got, "[/active rules]\n\n[memory unavailable]") {
		t.Fatalf("blocks must stay separated by a blank line:\n%s", got)
	}
}

func TestUnavailableGuidanceIgnoresNilBuilder(t *testing.T) {
	appendMemoryUnavailableGuidance(nil) // must not panic
}

// memoryNoMatch keys off the rendered marker, so an empty render reads as
// "no marker" rather than "no match". That is why a failed lookup could
// not be detected from the traces alone and needs an explicit status.
func TestMemoryNoMatchCannotDetectAFailedLookup(t *testing.T) {
	if memoryNoMatch("") {
		t.Fatal("an empty render is not a no-match signal")
	}
	if !memoryNoMatch("[retrieval]\nno_match") {
		t.Fatal("the explicit marker must still be recognised")
	}
}
