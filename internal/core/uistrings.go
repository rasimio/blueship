package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

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
	// ExecutionDenied is sent by interactive transports when the host's
	// generic execution policy denies a turn.
	ExecutionDenied string
	// ResetDone and ResetFailed acknowledge the /reset command.
	ResetDone   string
	ResetFailed string

	// StopButton labels the control a chat transport attaches to an answer
	// while it is still being written.
	StopButton string
	// StopAcknowledged is the toast shown when a stop is accepted.
	StopAcknowledged string
	// StopNothingRunning answers a stop for a conversation that is idle —
	// the turn ended between the tap and its delivery, or the command was
	// typed out of the blue.
	StopNothingRunning string
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
	if u.ExecutionDenied == "" {
		u.ExecutionDenied = "This assistant is not available right now."
	}
	if u.ResetDone == "" {
		u.ResetDone = "Session reset. New thread."
	}
	if u.ResetFailed == "" {
		u.ResetFailed = "Reset failed."
	}
	if u.StopButton == "" {
		u.StopButton = "Stop"
	}
	if u.StopAcknowledged == "" {
		u.StopAcknowledged = "Stopping…"
	}
	if u.StopNothingRunning == "" {
		u.StopNothingRunning = "Nothing is being written right now."
	}
}

// BotCommand is one entry in the Telegram command menu.
//
// Two kinds live in the same list, distinguished by Prompt:
//
//   - Prompt empty — a command something already handles (/start, /reset,
//     /persona). Listing it only makes it discoverable; the menu is the
//     only place a user ever learns these exist.
//   - Prompt set — a prompt shortcut. The command is rewritten to Prompt
//     and dispatched as though the user had typed it, so the answer is an
//     ordinary turn with ordinary tools rather than a static help page
//     that starts drifting from the truth the day it is written.
//   - Host set — the host answers it directly, through
//     Deps.BotCommandHandler, without a model turn.
//
// Name and Description are host copy in the host's language; the
// framework has no commands and no words of its own.
type BotCommand struct {
	// Name without the leading slash, lowercase — Telegram rejects
	// anything else.
	Name        string
	Description string
	Prompt      string

	// Host routes the command to Deps.BotCommandHandler instead of to a
	// model turn. Mutually exclusive with Prompt.
	//
	// Host commands are admitted BEFORE the execution policy runs, which
	// is the point of having them: the case they exist for is a person
	// who has been refused a turn and needs a way to do something about
	// it. Routing "let me pay" through the same gate that just said no
	// would be a closed loop.
	Host bool
}

// BotCommandRequest is one host-handled command invocation.
type BotCommandRequest struct {
	Name   string // without the leading slash
	Args   string // everything after the command, trimmed
	UserID uuid.UUID
	SoulID uuid.UUID
}

// BotCommandResult is what the host wants said back. Text is required;
// ButtonURL adds a single link button under it, which is how a host
// hands over something the chat cannot render itself.
type BotCommandResult struct {
	Text        string
	ButtonLabel string
	ButtonURL   string

	// AwaitReply, when set, claims the person's next message and delivers
	// it back to the handler as the Args of a command by this name.
	//
	// This is what lets a host ask one question without inventing its own
	// state: "type your email" is a step, and a step needs somewhere to
	// remember that it is outstanding. The framework already keeps
	// per-(user, bot) state for onboarding and reuses it here rather than
	// asking every host to build the same thing.
	AwaitReply string
}

// BotCommandHandler answers host-handled commands. Nil means no command
// is host-handled, whatever the config says.
type BotCommandHandler func(context.Context, BotCommandRequest) (BotCommandResult, error)

// OnboardingMode selects what a fresh /start on an unpaired chat does.
type OnboardingMode string

const (
	// OnboardingModeWizard asks the user to compose a persona before the
	// account exists: name → voice → traits → description → confirm. The
	// user does the configuring first and reaches the assistant last.
	OnboardingModeWizard OnboardingMode = "wizard"

	// OnboardingModeInstant mints the account on first contact using the
	// flow's default persona and starts the conversation immediately.
	// Configuring the persona becomes a later, optional step.
	//
	// The trade is deliberate and worth stating: instant mode creates a
	// tenant for anyone who taps /start, including the large share who
	// send nothing afterwards. A host that pays per tenant should weigh
	// that against what the wizard costs it at the top of the funnel.
	OnboardingModeInstant OnboardingMode = "instant"
)

// OnboardingSeedButton is one opening move offered on first contact.
// Label is what the user reads; Text is dispatched as though the user had
// typed it, so the assistant's reply is an ordinary turn with ordinary
// memory and tools rather than a canned script.
type OnboardingSeedButton struct {
	Label string
	Text  string
}

// OnboardingFlow parameterises chat-native signup.
//
// Everything here is host-supplied, including the persona vocabulary
// itself. The framework has no opinion about which voices or character
// traits exist: that list is a product taxonomy, it has to be worded in
// the product's language, and — decisively — the ids end up persisted on
// the host's own rows, where the host's other surfaces read them back. A
// framework shipping its own palette would be dictating the contents of a
// table it does not own.
type OnboardingFlow struct {
	// Mode defaults to OnboardingModeWizard when empty.
	Mode OnboardingMode

	// Voices and Traits are the persona palette the pickers render, in
	// the host's own language.
	//
	// Id and label are separate throughout, and that separation is
	// load-bearing rather than tidy: the id is what gets persisted and
	// handed back on completion, so other surfaces of the host read it
	// back and it must not move, while the label is only ever displayed
	// and a host may reword or translate it with no migration.
	//
	// Required in both modes — the wizard renders them as pickers, and
	// instant mode draws its defaults from them.
	Voices []OnboardingVoice
	Traits []OnboardingTrait

	// Input bounds. Zero takes the conventional default; a host with its
	// own form validation sets these to match, so chat and web refuse the
	// same inputs.
	NameMinRunes        int
	NameMaxRunes        int
	MaxTags             int
	MaxDescriptionRunes int

	// DefaultName, DefaultVoice and DefaultTags are the persona an
	// instant-mode account is born with. The name must satisfy the bounds
	// above, and the voice and tags must come from the palette — an
	// instant flow that fails these is refused at startup rather than
	// minting broken personas.
	DefaultName  string
	DefaultVoice string
	DefaultTags  []string

	// Welcome is the single message an instant-mode chat opens with.
	Welcome string

	// OfferPersonaAfterTurns is how many exchanges pass before the chat
	// offers to personalise the assistant. Zero disables the offer.
	//
	// The offer is deferred rather than shown up front on purpose: the
	// same question that reads as a chore before the first reply reads as
	// an invitation once someone is already talking, and it gives a
	// first-day user a reason to come back. Count it low enough that most
	// people who engage at all reach it.
	OfferPersonaAfterTurns int

	// SeedButtons are attached to Welcome. Empty is allowed — the chat
	// then opens with prose alone and waits for the user to write.
	SeedButtons []OnboardingSeedButton
}

// OnboardingVoice is one option in the persona voice palette. ID is the
// token persisted on the persona row and handed back to the host on
// completion; Name and Desc are what the user reads, already in whatever
// language the host speaks. Desc is optional.
type OnboardingVoice struct {
	ID   string
	Name string
	Desc string
}

// OnboardingTrait is one option in the character-trait palette. Same
// split as OnboardingVoice: ID is persisted, Label is displayed.
type OnboardingTrait struct {
	ID    string
	Label string
}

// Conventional bounds, applied when the flow leaves them at zero. They
// are round numbers chosen to fit a Telegram message, not a rule derived
// from any product — a host with a stricter form sets its own.
const (
	defaultOnboardingNameMinRunes   = 2
	defaultOnboardingNameMaxRunes   = 30
	defaultOnboardingMaxTags        = 5
	defaultOnboardingMaxDescription = 400
)

// NameBounds, TagCap and DescriptionCap resolve the configured limits,
// substituting the conventional defaults for anything left at zero.
// Exported because the pickers that enforce them live in the transport
// package, not here.
func (f OnboardingFlow) NameBounds() (minRunes, maxRunes int) {
	minRunes, maxRunes = f.NameMinRunes, f.NameMaxRunes
	if minRunes <= 0 {
		minRunes = defaultOnboardingNameMinRunes
	}
	if maxRunes <= 0 {
		maxRunes = defaultOnboardingNameMaxRunes
	}
	return minRunes, maxRunes
}

func (f OnboardingFlow) TagCap() int {
	if f.MaxTags <= 0 {
		return defaultOnboardingMaxTags
	}
	return f.MaxTags
}

func (f OnboardingFlow) DescriptionCap() int {
	if f.MaxDescriptionRunes <= 0 {
		return defaultOnboardingMaxDescription
	}
	return f.MaxDescriptionRunes
}

// HasVoice and HasTrait report palette membership.
func (f OnboardingFlow) HasVoice(id string) bool {
	for _, v := range f.Voices {
		if v.ID == id {
			return true
		}
	}
	return false
}

func (f OnboardingFlow) HasTrait(id string) bool {
	for _, t := range f.Traits {
		if t.ID == id {
			return true
		}
	}
	return false
}

// TraitLabel returns a trait's display label, falling back to the id so a
// half-filled palette degrades to something readable rather than blank.
func (f OnboardingFlow) TraitLabel(id string) string {
	for _, t := range f.Traits {
		if t.ID == id && t.Label != "" {
			return t.Label
		}
	}
	return id
}

// Validate reports why this flow cannot run, or nil.
//
// Called at startup so a palette typo surfaces as a refused boot rather
// than as tenants whose persona row points at a voice that does not
// exist. Both modes need a palette — the wizard renders it as pickers,
// instant mode draws its defaults from it — so those checks are common;
// only the opening copy is instant-specific.
func (f OnboardingFlow) Validate() error {
	if len(f.Voices) == 0 {
		return errors.New("onboarding flow: no voices — the framework ships no persona vocabulary, the host supplies it")
	}
	seen := make(map[string]bool, len(f.Voices))
	for i, v := range f.Voices {
		switch {
		case strings.TrimSpace(v.ID) == "":
			return fmt.Errorf("onboarding flow: voice %d has no id", i)
		case strings.TrimSpace(v.Name) == "":
			return fmt.Errorf("onboarding flow: voice %q has no name", v.ID)
		case seen[v.ID]:
			return fmt.Errorf("onboarding flow: voice id %q appears twice", v.ID)
		}
		seen[v.ID] = true
	}
	seenTrait := make(map[string]bool, len(f.Traits))
	for i, t := range f.Traits {
		switch {
		case strings.TrimSpace(t.ID) == "":
			return fmt.Errorf("onboarding flow: trait %d has no id", i)
		case strings.TrimSpace(t.Label) == "":
			return fmt.Errorf("onboarding flow: trait %q has no label", t.ID)
		case seenTrait[t.ID]:
			return fmt.Errorf("onboarding flow: trait id %q appears twice", t.ID)
		}
		seenTrait[t.ID] = true
	}
	if minRunes, maxRunes := f.NameBounds(); minRunes > maxRunes {
		return fmt.Errorf("onboarding flow: name bounds inverted (%d > %d)", minRunes, maxRunes)
	}

	if f.Mode != OnboardingModeInstant {
		return nil
	}
	minRunes, maxRunes := f.NameBounds()
	if n := len([]rune(strings.TrimSpace(f.DefaultName))); n < minRunes || n > maxRunes {
		return fmt.Errorf("onboarding flow: instant mode needs a default name of %d-%d characters, got %q", minRunes, maxRunes, f.DefaultName)
	}
	if f.DefaultVoice == "" {
		return errors.New("onboarding flow: instant mode needs a default voice id")
	}
	if !f.HasVoice(f.DefaultVoice) {
		return fmt.Errorf("onboarding flow: default voice %q is not in the palette", f.DefaultVoice)
	}
	for _, tag := range f.DefaultTags {
		if !f.HasTrait(tag) {
			return fmt.Errorf("onboarding flow: default trait %q is not in the palette", tag)
		}
	}
	if strings.TrimSpace(f.Welcome) == "" {
		return errors.New("onboarding flow: instant mode needs a welcome message")
	}
	for i, b := range f.SeedButtons {
		if strings.TrimSpace(b.Label) == "" || strings.TrimSpace(b.Text) == "" {
			return fmt.Errorf("onboarding flow: seed button %d needs both a label and a text", i)
		}
	}
	return nil
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

	// Persona editing (the /persona command and the deferred offer).
	// Distinct from the creation copy because the user is renaming
	// someone they already know: "what should it be called" is the wrong
	// question to ask about an assistant they have been talking to.
	EditGreeting   string // shown when /persona starts a re-run
	EditDoneFmt    string // %s = new name
	OfferText      string // the deferred "want to personalise?" nudge
	BtnOfferSetup  string
	BtnOfferLater  string
	OfferLater     string // acknowledgement when the user defers
	ErrPersonaFail string // update failed; the soul is unchanged
	ErrNoPersona   string // editing is unavailable on this deployment

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
	d(&m.EditGreeting, "Let's reshape your assistant. What should it be called now?")
	d(&m.EditDoneFmt, "Done — say hello to %s.")
	d(&m.OfferText, "By the way — you can give me your own name and character. Takes half a minute.")
	d(&m.BtnOfferSetup, "Personalise")
	d(&m.BtnOfferLater, "Later")
	d(&m.OfferLater, "No problem. Send /persona whenever you feel like it.")
	d(&m.ErrPersonaFail, "Something went wrong saving that. Nothing was changed — try again.")
	d(&m.ErrNoPersona, "Personalising isn't available here yet.")
}
