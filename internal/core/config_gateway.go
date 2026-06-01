package core

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// GatewayConfig defines gateway behavior.
type GatewayConfig struct {
	DebounceWindow time.Duration // default: 1500ms
	DebounceCap    int           // default: 10
	MaxTurns       int           // default: 15

	// Debug, when true, both:
	//   - sends errors to the owner via Telegram instead of a "Sorry..." reply;
	//   - forces every turn to be followed by a debug.txt dump with AME
	//     traces, reflex guidance, rule matches, and tool calls.
	// Per-user /debug toggles still work on top of this (they only matter
	// when the config flag is off).
	Debug bool

	// InteractionTier, when true, runs the two-tier interaction model:
	// the Reflex (fast) tier handles every turn and decides — via the
	// `escalate` tool — when to hand off to the Cortex (background) tier.
	// When false the gateway calls Cortex directly every turn (legacy).
	// Default false; opt-in for safe rollout.
	InteractionTier bool

	// SkipReflexOnText, when true, skips the Reflex fast tier on text
	// transports (those that wire no spoken-filler flush — i.e. onReflexDone
	// is nil) and runs Cortex directly, while voice transports keep the
	// two-tier reflex+filler. Rationale (Phase 2, calibration-backed): on a
	// single base model the reflex pre-pass in text neither masks latency
	// (streaming Cortex is its own handshake) nor reliably routes (a cheap
	// difficulty gate mis-routes ~half the hard turns), so it is net overhead
	// in text; voice still benefits from the sub-second filler. Default false.
	SkipReflexOnText bool

	// BargeIn, when true, runs the WebSocket voice transport with a
	// concurrent read loop + turn manager so the user can interrupt a
	// response mid-stream (speech_start / cancel frames). When false the
	// transport uses the legacy strictly-sequential read loop.
	// Default false; opt-in, paired with the client-side AEC work.
	BargeIn bool

	// TurnCompletedHook fires after the gateway successfully sends an
	// assistant reply to the user (across Telegram batch, Telegram
	// streaming, voice streaming, and WebSocket batch transports). The
	// host (e.g. a sleep-time agent) uses this signal to
	// drive per-user state machines without polling. The hook runs in a
	// goroutine inside the gateway, so a slow callback doesn't add
	// latency to the response path. Nil = no-op.
	TurnCompletedHook func(ctx context.Context, userID, sessionID uuid.UUID) `yaml:"-" json:"-"`

	// AgentIterationCompletedHook is the agent_task analogue of
	// TurnCompletedHook: it fires after every successful (non-error)
	// iteration of an agent_task, regardless of strategy or terminal
	// state (Pause / Done / continue). Hosts use this to attach a
	// write-time pipeline (e.g. a write-time saver) so research
	// artifacts produced inside background iterations land in the same
	// obligatory memory pipeline as chat-turn findings — without asking
	// the LLM to call memory_save inside the agent loop. Runs in a
	// goroutine; receiver is responsible for its own DB/embedding work.
	// Nil = no-op.
	AgentIterationCompletedHook func(ctx context.Context, task AgentTask, result IterationResult) `yaml:"-" json:"-"`

	// ResolveSoul maps an already-resolved user to the soul that should
	// handle their request. The gateway calls it after user resolution
	// and threads the result through ctx via WithSoulID so every
	// downstream write is tenant-attributed. The host supplies the
	// implementation (typically a membership-graph lookup); blueship
	// stays generic about routing. Nil on a tenant-bound deployment is
	// a misconfiguration — writes then land with a Nil soul and fail
	// the FK constraint.
	ResolveSoul func(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) `yaml:"-" json:"-"`

	// ResolveTelegramChat maps a (bot, Telegram chat) pair to its bound
	// (user, soul). The gateway calls it on every inbound Telegram update.
	// Host-implemented (typically a vaelum.bot_links lookup); blueship
	// stays generic.
	//
	// Return ErrTelegramChatUnpaired (or any error whose Is-chain reaches
	// it) to indicate "no link" — the gateway then runs the unpaired-chat
	// policy (greet+signup on platform bots, ignore on user bots). Other
	// errors are treated as transient and the message is dropped with a
	// warning log.
	ResolveTelegramChat func(ctx context.Context, botID uuid.UUID, tgChatID int64) (userID, soulID uuid.UUID, err error) `yaml:"-" json:"-"`

	// AttachmentSink, when set, receives every photo / document a
	// transport produces (Telegram, web cabinet uploads via the
	// gateway path, …). Ship.Run propagates this into deps so the
	// gateway can call it on every turn after the session id is
	// known. Nil = no CDN, the LLM still sees the bytes inline.
	AttachmentSink AttachmentSink `yaml:"-" json:"-"`

	// BotOnboarding, when set, drives a fresh /start on an unpaired
	// Telegram chat through inline account creation (FSM in the chat,
	// no website round-trip). The gateway detects the no-identity case,
	// runs the host-supplied state machine via this hook, finalises
	// with CreateAccount, and only then hands control to the cortex
	// turn. Nil falls back to the legacy replyUnpaired greeting.
	BotOnboarding BotOnboarding `yaml:"-" json:"-"`

	// Onboarding holds the chat-native onboarding UI copy. Generic English
	// defaults are filled by ApplyDefaults; a host overrides to brand it.
	Onboarding OnboardingMessages `yaml:"-" json:"-"`

	// ResolveUserBotID maps a (user, Telegram chat) to the bot id that should
	// deliver to them, for hosts that run multiple bots per user. Host-supplied
	// (typically a bot-pairing lookup). Nil disables per-user bot routing in
	// SendToUser. The framework owns no platform schema.
	ResolveUserBotID func(ctx context.Context, userID uuid.UUID, tgChatID int64) (uuid.UUID, error) `yaml:"-" json:"-"`

	// ResolveDisplayName returns a friendly display name for a user, or "" if
	// none is known. Host-supplied; nil = the caller falls back to a generic
	// noun rather than a raw chat id.
	ResolveDisplayName func(ctx context.Context, userID uuid.UUID) string `yaml:"-" json:"-"`

	// ResolveSoulToolPolicy returns a soul's per-tool enable/disable overrides
	// and its connected service providers, so the gateway can compute the
	// per-turn tool allowlist. Host-supplied (typically a cabinet lookup). Nil
	// or an error means "no filtering — every registered tool is allowed".
	ResolveSoulToolPolicy func(ctx context.Context, soulID uuid.UUID) (overrides map[string]bool, connectedProviders []string, err error) `yaml:"-" json:"-"`

	// ResolveSoulPersona returns a soul's persona/system-prompt text. Required
	// when souls are used (non-nil soulID) — the framework has no persona store
	// of its own; a nil hook is a misconfiguration for a soul-bound deployment.
	ResolveSoulPersona func(ctx context.Context, soulID uuid.UUID) (string, error) `yaml:"-" json:"-"`

	// ResolvePlatformPrompts returns the platform-wide preamble + agents prompt
	// layers composed around each soul's persona. Host-supplied; the gateway
	// caches the result for the process lifetime. Required when souls are used.
	ResolvePlatformPrompts func(ctx context.Context) (preamble, agents string, err error) `yaml:"-" json:"-"`
}
