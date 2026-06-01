package core

// UIStrings holds the framework-emitted, human-language strings a host may
// localize. BlueShip ships generic English defaults (filled by ApplyDefaults);
// a host overrides any field to speak its own language/persona. The framework
// itself owns no tenant- or language-specific text — that is a host concern.
type UIStrings struct {
	// ModelRefused is shown to the user when the model declines to answer
	// and produced no text (so the turn isn't silent).
	ModelRefused string
	// InterruptMarker replaces an assistant message that the user cut off
	// before it finished (stored in history and shown back).
	InterruptMarker string
	// InterruptSuffix is appended to a partial assistant message that was
	// interrupted mid-stream.
	InterruptSuffix string
	// NoActiveNotes is injected into reflex/cortex context when the user has
	// no open notes.
	NoActiveNotes string
}

func (u *UIStrings) applyDefaults() {
	if u.ModelRefused == "" {
		u.ModelRefused = "(the model declined to answer this request — rephrase / simplify the context)"
	}
	if u.InterruptMarker == "" {
		u.InterruptMarker = "[interrupted by user]"
	}
	if u.InterruptSuffix == "" {
		u.InterruptSuffix = " […interrupted]"
	}
	if u.NoActiveNotes == "" {
		u.NoActiveNotes = "(no active notes)"
	}
}

// OnboardingMessages bundles the chat-native onboarding UI copy. Kept as a
// struct so a host swaps the whole locale/persona in one place. BlueShip
// ships generic English defaults; a host (e.g. a branded platform) overrides
// them via GatewayConfig.Onboarding. %s/%d placeholders must be preserved.
type OnboardingMessages struct {
	Greeting            string // shown on a fresh /start
	NamePromptFmt       string // %s = name
	NameTooShort        string
	VoicePromptFmt      string // %s = name
	TraitsPrompt        string
	TraitsCounterFmt    string // %d = selected count
	DescriptionPrompt   string
	ConfirmTitle        string
	ConfirmRowFmt       string // %s = label, %s = value
	ConfirmTrueQ        string
	BtnConfirmOK        string
	BtnConfirmBack      string
	WorkingFmt          string // %s = name
	DoneFmt             string // %s = name
	BackFmt             string // %s = name (welcome-back)
	ErrAccountFail      string
	ErrAlreadyOnboarded string
	DashEmpty           string // shown for empty tags / description
	FallbackName        string // used when a name is missing
	LabelName           string // confirm-row label
	LabelVoice          string // confirm-row label
	LabelTraits         string // confirm-row label
	LabelDescription    string // confirm-row label
}

func (m *OnboardingMessages) applyDefaults() {
	d := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	d(&m.Greeting, "Hi, I'm here to help you set up your own assistant. What should it be called?")
	d(&m.NamePromptFmt, "Nice to meet you, %s. Pick a voice:")
	d(&m.NameTooShort, "The name must be 2–30 characters. Try again:")
	d(&m.VoicePromptFmt, "Nice to meet you, %s. Pick a voice:")
	d(&m.TraitsPrompt, "Pick up to 5 personality traits. Tap to select, tap again to deselect. When ready, press Done.")
	d(&m.TraitsCounterFmt, "Done · %d of 5")
	d(&m.DescriptionPrompt, "Or describe the character in your own words (one line), or send /skip to skip.")
	d(&m.ConfirmTitle, "Let's check before creating:")
	d(&m.ConfirmRowFmt, "%s: %s")
	d(&m.ConfirmTrueQ, "All correct?")
	d(&m.BtnConfirmOK, "✓ Create")
	d(&m.BtnConfirmBack, "← Back")
	d(&m.WorkingFmt, "Setting things up...")
	d(&m.DoneFmt, "Done — meet your %s. Say something and it'll reply.")
	d(&m.BackFmt, "Welcome back, %s!")
	d(&m.ErrAccountFail, "Something went wrong creating the account. Try again — press ✓ Create.")
	d(&m.ErrAlreadyOnboarded, "You already have an assistant. Say something and it'll reply.")
	d(&m.DashEmpty, "—")
	d(&m.FallbackName, "friend")
	d(&m.LabelName, "Name")
	d(&m.LabelVoice, "Voice")
	d(&m.LabelTraits, "Traits")
	d(&m.LabelDescription, "Description")
}
