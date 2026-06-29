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

func TestFormatRulesAsGuidanceSkipsSuppressedRules(t *testing.T) {
	got := formatRulesAsGuidance([]bs.ActiveRule{
		{
			Trigger:          "suppressed trigger",
			Action:           "suppressed action",
			MatchType:        "legacy_keyword",
			Scope:            "legacy_keyword",
			Disposition:      "suppressed",
			Suppressed:       true,
			SuppressedReason: "lower priority duplicate",
		},
		{
			Trigger:          "active trigger",
			Action:           "active action",
			MatchType:        "legacy_keyword",
			Scope:            "legacy_keyword",
			Disposition:      "primary",
			Anchor:           "взлетай",
			EligibilityScore: 0.96,
		},
	})

	if strings.Contains(got, "suppressed trigger") || strings.Contains(got, "suppressed action") {
		t.Fatalf("formatted rules included suppressed rule:\n%s", got)
	}
	if !strings.Contains(got, `RULE #1 (match=legacy_keyword; scope=legacy_keyword; disposition=primary; anchor="взлетай"; score=0.96)`) {
		t.Fatalf("formatted rules missing active arbitration metadata:\n%s", got)
	}
}
