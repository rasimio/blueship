package gateway

import (
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestFormatRulesAsGuidanceIncludesMatchMetadata(t *testing.T) {
	got := formatRulesAsGuidance([]bs.ActiveRule{{
		Trigger:   "Расим говорит 'взлетай'",
		Action:    "Ответить автономно.",
		MatchType: "legacy_keyword",
		Scope:     "legacy_keyword",
		Reason:    "keyword: взлетай",
		Tools:     []string{"browser_search"},
	}})

	want := `RULE #1 (match=legacy_keyword; scope=legacy_keyword; reason="keyword: взлетай")`
	if !strings.Contains(got, want) {
		t.Fatalf("formatted rules missing metadata header %q:\n%s", want, got)
	}
	if !strings.Contains(got, "TOOLS: browser_search") {
		t.Fatalf("formatted rules missing tools line:\n%s", got)
	}
}
