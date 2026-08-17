package core

import (
	"strings"
	"testing"
)

// A host-supplied palette, deliberately not the one any product uses —
// these tests must not encode a vocabulary the framework no longer owns.
func testPalette() ([]OnboardingVoice, []OnboardingTrait) {
	return []OnboardingVoice{
			{ID: "v1", Name: "First", Desc: "The first one."},
			{ID: "v2", Name: "Second"},
		},
		[]OnboardingTrait{
			{ID: "t1", Label: "one"},
			{ID: "t2", Label: "two"},
			{ID: "t3", Label: "three"},
		}
}

func validWizardFlow() OnboardingFlow {
	voices, traits := testPalette()
	return OnboardingFlow{Mode: OnboardingModeWizard, Voices: voices, Traits: traits}
}

func validInstantFlow() OnboardingFlow {
	f := validWizardFlow()
	f.Mode = OnboardingModeInstant
	f.DefaultName = "Default"
	f.DefaultVoice = "v1"
	f.DefaultTags = []string{"t1", "t2"}
	f.Welcome = "Hi."
	f.SeedButtons = []OnboardingSeedButton{{Label: "Try it", Text: "Tell me something."}}
	return f
}

// The palette is required by both modes, because both render it: the
// wizard as pickers, instant mode as the source of its defaults. A flow
// without one produces a voice prompt carrying zero buttons, which strands
// the user with no way forward and nothing logged.
func TestOnboardingFlowRequiresAPalette(t *testing.T) {
	for name, flow := range map[string]OnboardingFlow{
		"zero value":      {},
		"wizard, no data": {Mode: OnboardingModeWizard},
		"instant, no data": {
			Mode: OnboardingModeInstant, DefaultName: "Default", Welcome: "Hi.",
		},
	} {
		err := flow.Validate()
		if err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), "no voices") {
			t.Errorf("%s: Validate() = %q, want it to mention the missing palette", name, err)
		}
	}
}

// Beyond the palette the wizard needs nothing: every remaining field is
// collected from the user mid-conversation and validated as it arrives.
func TestOnboardingFlowWizardNeedsOnlyAPalette(t *testing.T) {
	if err := validWizardFlow().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestOnboardingFlowInstantAcceptsAWellFormedFlow(t *testing.T) {
	if err := validInstantFlow().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// A malformed palette is worth catching on its own: these are states the
// pickers cannot render coherently, and a duplicate id in particular makes
// one option permanently unreachable while looking fine in config.
func TestOnboardingFlowRejectsAMalformedPalette(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*OnboardingFlow)
		want   string
	}{
		"voice without id":    {func(f *OnboardingFlow) { f.Voices[0].ID = "" }, "no id"},
		"voice without name":  {func(f *OnboardingFlow) { f.Voices[1].Name = "" }, "no name"},
		"duplicate voice id":  {func(f *OnboardingFlow) { f.Voices[1].ID = "v1" }, "appears twice"},
		"trait without id":    {func(f *OnboardingFlow) { f.Traits[1].ID = "  " }, "no id"},
		"trait without label": {func(f *OnboardingFlow) { f.Traits[1].Label = "" }, "no label"},
		"duplicate trait":     {func(f *OnboardingFlow) { f.Traits[2].ID = "t1" }, "appears twice"},
		"inverted bounds":     {func(f *OnboardingFlow) { f.NameMinRunes, f.NameMaxRunes = 40, 10 }, "inverted"},
	} {
		flow := validWizardFlow()
		test.mutate(&flow)
		err := flow.Validate()
		if err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: Validate() = %q, want it to mention %q", name, err, test.want)
		}
	}
}

// Every one of these would otherwise reach production as tenant rows or a
// chat that opens on an empty message. None is visible without reading the
// config, which is why they are a refused boot rather than a warning.
func TestOnboardingFlowInstantRejectsUnusableConfig(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*OnboardingFlow)
		want   string
	}{
		"no name":           {func(f *OnboardingFlow) { f.DefaultName = "" }, "default name"},
		"name too short":    {func(f *OnboardingFlow) { f.DefaultName = "M" }, "default name"},
		"name too long":     {func(f *OnboardingFlow) { f.DefaultName = strings.Repeat("м", 31) }, "default name"},
		"whitespace name":   {func(f *OnboardingFlow) { f.DefaultName = "   " }, "default name"},
		"no voice":          {func(f *OnboardingFlow) { f.DefaultVoice = "" }, "default voice id"},
		"voice off palette": {func(f *OnboardingFlow) { f.DefaultVoice = "v9" }, "not in the palette"},
		"trait off palette": {func(f *OnboardingFlow) { f.DefaultTags = []string{"t1", "t9"} }, "not in the palette"},
		"no welcome":        {func(f *OnboardingFlow) { f.Welcome = "" }, "welcome message"},
		"blank welcome":     {func(f *OnboardingFlow) { f.Welcome = " \n " }, "welcome message"},
		"seed no label":     {func(f *OnboardingFlow) { f.SeedButtons[0].Label = "" }, "no label"},
		"seed no text":      {func(f *OnboardingFlow) { f.SeedButtons[0].Text = "" }, "does nothing"},
		"seed blank text":   {func(f *OnboardingFlow) { f.SeedButtons[0].Text = "  " }, "does nothing"},
		// A button either speaks as the person or answers them. Both at
		// once is two buttons' worth of intent on one, and only one of
		// them can happen.
		"seed speaks and answers": {func(f *OnboardingFlow) { f.SeedButtons[0].Reply = "смотри" }, "speaks as the person and answers"},
	} {
		flow := validInstantFlow()
		test.mutate(&flow)
		err := flow.Validate()
		if err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: Validate() = %q, want it to mention %q", name, err, test.want)
		}
	}
}

// The name bound is measured in runes, not bytes: a 30-character Cyrillic
// name is 60 bytes, and a byte-based check would refuse names a host's own
// web form accepts from the same users.
func TestOnboardingFlowNameBoundCountsRunes(t *testing.T) {
	flow := validInstantFlow()
	flow.DefaultName = strings.Repeat("м", 30)
	if err := flow.Validate(); err != nil {
		t.Fatalf("30 Cyrillic runes rejected: %v", err)
	}
}

// Bounds are conventional defaults, not framework policy — a host with
// its own form validation overrides them so chat and web agree.
func TestOnboardingFlowBoundsAreOverridable(t *testing.T) {
	flow := validInstantFlow()
	flow.NameMinRunes, flow.NameMaxRunes = 1, 4
	flow.DefaultName = "Ая"
	if err := flow.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a name inside the host's own bounds", err)
	}

	if minRunes, maxRunes := flow.NameBounds(); minRunes != 1 || maxRunes != 4 {
		t.Errorf("NameBounds() = %d,%d, want the configured 1,4", minRunes, maxRunes)
	}
	flow.DefaultName = "Мирослава"
	if err := flow.Validate(); err == nil {
		t.Error("Validate() = nil for a name past the host's own maximum, want an error")
	}
}

func TestOnboardingFlowCapsFallBackToDefaults(t *testing.T) {
	flow := validWizardFlow()
	if minRunes, maxRunes := flow.NameBounds(); minRunes != 2 || maxRunes != 30 {
		t.Errorf("NameBounds() = %d,%d, want the 2,30 defaults", minRunes, maxRunes)
	}
	if got := flow.TagCap(); got != 5 {
		t.Errorf("TagCap() = %d, want the default 5", got)
	}
	if got := flow.DescriptionCap(); got != 400 {
		t.Errorf("DescriptionCap() = %d, want the default 400", got)
	}

	flow.MaxTags, flow.MaxDescriptionRunes = 3, 120
	if got := flow.TagCap(); got != 3 {
		t.Errorf("TagCap() = %d, want the configured 3", got)
	}
	if got := flow.DescriptionCap(); got != 120 {
		t.Errorf("DescriptionCap() = %d, want the configured 120", got)
	}
}

// A host that supplies no seed buttons gets prose and a blinking cursor.
// That is a legitimate choice, not a misconfiguration.
func TestOnboardingFlowInstantAllowsNoSeedButtons(t *testing.T) {
	flow := validInstantFlow()
	flow.SeedButtons = nil
	if err := flow.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// The label is display-only and the id is what persists. A host that
// retranslates its palette must not change what any existing persona row
// means — this is the property that makes retranslation free.
func TestOnboardingFlowTraitLabelIsDisplayOnly(t *testing.T) {
	flow := validWizardFlow()
	if got := flow.TraitLabel("t2"); got != "two" {
		t.Errorf("TraitLabel(t2) = %q, want %q", got, "two")
	}
	flow.Traits[1].Label = "второй"
	if got := flow.TraitLabel("t2"); got != "второй" {
		t.Errorf("TraitLabel(t2) after retranslation = %q, want %q", got, "второй")
	}
	if !flow.HasTrait("t2") {
		t.Error("HasTrait(t2) = false after retranslating its label — the id must be what matches")
	}
	if flow.HasTrait("второй") {
		t.Error("HasTrait matched a label; only ids may match, or a retranslation would orphan stored personas")
	}
}

// An id with no label should still render as something.
func TestOnboardingFlowTraitLabelFallsBackToID(t *testing.T) {
	flow := validWizardFlow()
	if got := flow.TraitLabel("unknown"); got != "unknown" {
		t.Errorf("TraitLabel(unknown) = %q, want the id back", got)
	}
}
