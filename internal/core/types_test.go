package core

import (
	"strings"
	"testing"
)

func TestEstimateTextTokensConservativeForHebrew(t *testing.T) {
	text := strings.Repeat("שלום עולם ", 100)

	got := EstimateTextTokens(text)
	wantAtLeast := (len(text) + 1) / 2

	if got < wantAtLeast {
		t.Fatalf("Hebrew estimate should be at least bytes/2: got %d, want >= %d", got, wantAtLeast)
	}
	if old := len([]rune(text)) / 3; got <= old {
		t.Fatalf("Hebrew estimate should exceed old rune/3 heuristic: got %d, old %d", got, old)
	}
}

func TestEstimateTextTokensKeepsASCIICompact(t *testing.T) {
	if got := EstimateTextTokens("abcdef"); got != 2 {
		t.Fatalf("ASCII estimate changed unexpectedly: got %d, want 2", got)
	}
}
