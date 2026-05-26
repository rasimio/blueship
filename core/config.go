package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Config controls BlueShip runtime behavior.
// All fields have sensible defaults; only LLM, Transport, and DB are required.
type Config struct {
	// --- Required ---
	LLM        CompletionProvider // e.g. blueship.Anthropic(apiKey)
	Transport  TransportConfig    // e.g. blueship.Telegram(botToken)
	DB         string             // PostgreSQL DSN (app database)
	ShipSchema string             // Schema for BlueShip tables (default: "" = public)

	// --- Optional providers (nil = disabled) ---
	Embedder          EmbeddingProvider                // default: nil (embedding features disabled)
	Search            SearchEngine                     // default: nil (web_search tool disabled)
	Fetcher           WebFetcher                       // default: nil (auto-created if nil)
	Calendar          CalendarProvider                 // default: nil
	Transcriber       TranscriptionProvider            // default: nil (voice disabled)
	TTS               TTSProvider                      // default: nil (text-to-speech disabled)
	TTSVoice          string                           // default TTS voice name
	TTSInstructMapper func(strategy string) string     // maps emotion strategy to TTS instruct
	TTSTextCleaner    func(text string) string         // strips kaomoji/markdown for TTS
	TTSConverter      func(wav []byte) ([]byte, error) // WAV→OGG converter (nil = send WAV as-is)
	Sender            MessageSender                    // default: nil (message sending disabled)

	// --- Optional infrastructure ---
	Redis    string // Redis address (default: "" = no cache)
	Prompts  string // directory of <key>.md prompt files (required for personality)
	Timezone string // default: "UTC"

	// TraceFilePath, when set, enables the trace_recall tool. Points to the
	// jsonl file written by the OTel "file" exporter. Empty disables the
	// tool — there is no other source for self-observation, so omitting the
	// path means the agent literally cannot see its own work.
	TraceFilePath string

	// Logger lets the host pass in a pre-configured *slog.Logger so the
	// framework can join the host's log chain (JSON output, telemetry
	// alerter, trace_id correlation). Nil = blueship builds a TextHandler
	// to stderr — fine for examples, never for production.
	Logger *slog.Logger

	// SystemPromptKeys defines prompt keys that compose the system prompt.
	// Each key resolves to <key>.md inside Config.Prompts.
	// Default: ["preamble", "soul", "agents"]
	SystemPromptKeys []string

	// RoleTools maps a role name (cortex / reflex / background / …) to the
	// ordered tool allowlist for that role. Roles absent from the map fall
	// back to "no allowlist" — every registered tool is available.
	RoleTools map[string][]string

	// ToolMeta carries per-tool cabinet metadata (display category + the
	// `core` flag marking internal machinery users may not disable).
	// Supplied by the host; nil disables tool-catalog publishing and
	// per-soul tool filtering — generic consumers are unaffected.
	ToolMeta map[string]ToolMeta

	// MCPSource supplies a soul's external MCP-server tools to the gateway.
	// Supplied by the host; nil disables MCP entirely.
	MCPSource MCPToolSource

	// --- Owner (single-user mode) ---
	Owner OwnerConfig

	// --- Fine-tuning (all have defaults) ---
	Models   ModelsConfig
	Limits   LimitsConfig
	Timeouts TimeoutsConfig
	Retry    RetryConfig
	Gateway  GatewayConfig
	A2A      A2AConfig
	Fleet    FleetConfig
}

// ToolMeta is per-tool metadata the host supplies for the Vaelum cabinet:
// the display category, whether the tool is core machinery (always on, not
// user-disablable), and an optional Provider. A non-empty Provider binds
// the tool to a connected service integration — the gateway hides it from
// any soul that has not connected that provider. Tools absent from
// Config.ToolMeta default to category "general", non-core, no provider.
type ToolMeta struct {
	Category string
	Core     bool
	Provider string
}

// FleetConfig controls the optional BlueFleet directory integration.
//
// When Enabled, the ship registers itself at startup (publishes display
// name, description, capabilities, and the exposed tool catalog) and
// periodically refreshes a local cache of peers offering capabilities
// listed in InterestedIn. The Fleet path is purely additive — it does not
// yet replace A2A config-driven peers (that cutover happens in a later
// migration phase).
type FleetConfig struct {
	Enabled         bool
	BaseURL         string // e.g. "http://localhost:8500"
	ClientID        string // OAuth client_id issued by `bluefleet-admin register`
	ClientSecret    string // OAuth client_secret (env-sourced; do not commit)
	DisplayName     string // human-friendly name
	Description     string // short blurb shown in agent cards
	EndpointURL     string // where peers invoke tools on this Ship (usually A2A.BaseURL)
	PublicKey       string // PEM-encoded public key (optional)
	Capabilities    []FleetCapability
	InterestedIn    []string      // capability tags to search for peers
	RefreshInterval time.Duration // default 5 minutes
}

// FleetCapability is one declared ability of this agent. Tag is the
// discovery key peers search on; description is free-text.
type FleetCapability struct {
	Tag         string
	Description string
	Metadata    []byte // optional JSON metadata
}

// OwnerConfig identifies the single owner of this instance.
type OwnerConfig struct {
	ChatID      string // e.g. "telegram:5452235517"
	DisplayName string // e.g. "Alice"
}

// TransportConfig holds transport configuration.
type TransportConfig struct {
	Type     string // "telegram"

	// BotToken — DEPRECATED. Single-bot fallback path kept so existing
	// dev/test configs (and the legacy ArleneKateBot bootstrap) keep
	// working until the migration to multi-bot is complete. New deployments
	// supply Telegram.ListBots; the gateway then ignores BotToken with a
	// warning at startup.
	BotToken string

	// Telegram carries the multi-bot configuration. When ListBots is
	// non-nil the gateway maintains an in-memory registry of bots driven
	// by the host (typically a vaelum.bots SELECT), and ignores BotToken.
	Telegram TelegramConfig

	// WebSocket server for voice/desktop clients (runs alongside Telegram).
	WebSocket WebSocketConfig

	// HTTPChat server for the Vaelum web platform (runs alongside Telegram).
	HTTPChat HTTPChatConfig
}

// TelegramConfig configures the multi-bot Telegram transport.
//
// Vaelum is multi-tenant: the platform bot @VaelumBot routes messages to
// many souls via per-chat pairings, and any user can register their own
// bot token to get a dedicated entrypoint. The host owns persistence,
// authorisation, and token decryption — blueship is given a fully-realised
// list of (id, kind, owner, plaintext token) tuples and runs a polling
// goroutine per row.
type TelegramConfig struct {
	// ListBots is called at startup and on every reload to enumerate the
	// bots the gateway should poll. The host is responsible for decrypting
	// tokens before returning them; blueship never sees ciphertext.
	// Nil → fall back to single-bot Transport.BotToken (legacy).
	ListBots func(ctx context.Context) ([]BotConfig, error) `yaml:"-" json:"-"`

	// ReloadInterval controls how often the gateway polls ListBots in the
	// background (in addition to host-triggered reloads via Gateway.ReloadBots).
	// Default: 60 seconds. Zero or negative disables background reconcile.
	ReloadInterval time.Duration

	// ReloadTrigger is the host's poke channel. Sending an empty struct
	// asks the gateway to reconcile its registry against ListBots
	// immediately (cabinet add/delete signal). Nil = disabled; the
	// background reconcile still runs every ReloadInterval.
	ReloadTrigger chan struct{} `yaml:"-" json:"-"`
}

// BotConfig describes one Telegram bot the gateway should manage.
type BotConfig struct {
	// ID is the host's stable identifier for this bot (typically the
	// vaelum.bots.id UUID). The gateway uses it as a routing key and in
	// the pairing query (telegram_pairings.bot_id = $1). uuid.Nil is
	// reserved for the legacy BotToken fallback path.
	ID uuid.UUID

	// Kind discriminates routing semantics:
	//   "platform" — open to any signed-up Vaelum user; unpaired chats
	//                get a signup greeting.
	//   "user"     — paired only to the owner; unpaired chats are
	//                silently ignored.
	Kind string

	// OwnerUserID is the Vaelum user that owns a "user"-kind bot. For
	// "platform" bots it is uuid.Nil.
	OwnerUserID uuid.UUID

	// Token is the raw Telegram bot token used to authenticate against
	// the Bot API. Decrypted host-side; passed in plaintext to blueship.
	Token string
}

// WebSocketConfig configures the optional WebSocket server.
type WebSocketConfig struct {
	Port  int    // 0 = disabled
	Token string // legacy shared bearer token (dev fallback when ResolveDevice is nil)

	// ResolveDevice authenticates a per-user device bearer token and
	// returns the (user, soul) it is bound to. When non-nil, voice
	// connections present `Authorization: Bearer <opaque>` and are
	// dispatched to ProcessInboundForUser — no chatID translation, no
	// owner gate. When nil, the legacy shared Token + voice:owner
	// chatID path runs (single-tenant dev fallback). The host supplies
	// the implementation (typically a vaelum.devices lookup); blueship
	// stays generic about the token format.
	ResolveDevice func(ctx context.Context, token string) (userID, soulID uuid.UUID, err error) `yaml:"-" json:"-"`
}

// HTTPChatConfig configures the optional HTTP/SSE chat server that serves
// the Vaelum web platform's live chat.
type HTTPChatConfig struct {
	Port  int    // 0 = disabled
	Token string // bearer service token vaelum must present

	// Extras, when non-nil, is called once with the server's mux during
	// startup so the host (arlene daemon) can mount additional internal
	// API routes on the same port and share the bearer-token middleware.
	// Generic from blueship's side — it just calls the callback. Used for
	// arlene's `/api/internal/memory/associate` endpoint that proxies AME
	// search from the Vaelum cabinet.
	Extras func(*http.ServeMux) `yaml:"-" json:"-"`
}

// ModelRef identifies a model and its provider.
type ModelRef struct {
	Provider       string
	Name           string
	MaxTokens      int
	ThinkingBudget int
	Temperature    float64
}

// ForRouter returns "provider:name" for use with LLMRouter.
func (r ModelRef) ForRouter() string {
	if r.Provider != "" {
		return r.Provider + ":" + r.Name
	}
	return r.Name
}

// ModelsConfig defines which models to use for each role.
type ModelsConfig struct {
	Primary ModelRef // agent loop (default: "claude-haiku-4-5-20251001")
	Compact ModelRef // compaction summarizer (default: "claude-haiku-4-5-20251001")
}

// LimitsConfig defines token budget limits.
type LimitsConfig struct {
	MaxContext       int // Opus input budget (default: 180000)
	CompactThreshold int // trigger compaction above this (default: 40000)
	CompactKeep      int // keep recent messages intact (default: 30000)
	MaxOutputTokens  int // agent loop max output (default: 8192)
	CompactOutput    int // haiku compaction output (default: 2048)
	ThinkingBudget   int // extended thinking budget (default: 0 = disabled)
	MinMessageBudget int // minimum token budget for messages (default: 10000)
}

// TimeoutsConfig defines timeouts for external calls.
type TimeoutsConfig struct {
	LLM            time.Duration // main Claude call (default: 120s)
	Compact        time.Duration // Haiku compaction (default: 30s)
	Embedding      time.Duration // embedding API (default: 15s)
	Transcription  time.Duration // whisper (default: 30s)
	TelegramClient time.Duration // telegram sends (default: 10s)
	TelegramPoll   time.Duration // telegram long-poll (default: 35s)
}

// RetryConfig defines retry behavior for LLM calls.
type RetryConfig struct {
	MaxAttempts int             // default: 3
	Backoff     []time.Duration // default: [5s, 15s, 30s]
}

// A2AConfig controls the Agent-to-Agent protocol subsystem.
//
// When Enabled, the ship starts an HTTP server on Port that exposes
// /.well-known/agent (capability discovery), /a2a/invoke (tool dispatch),
// and /a2a/events (SSE stream for async results). Tools are only reachable
// through A2A if they were explicitly marked Exposed in the ToolRegistry.
type A2AConfig struct {
	Enabled   bool
	Name      string // ship identifier published in the agent card
	Version   string // semver or build tag
	Port      int    // 0 = disabled
	BaseURL   string // externally reachable URL for agent card self-reference
	AuthToken string // shared secret accepted on inbound requests

	// TraceChatID is the Telegram chat used for [a2a-trace] visibility
	// messages. Empty disables tracing; typical value is the group chat
	// where the owner watches inter-agent conversations.
	TraceChatID string
	TraceLevel  string // "off" | "errors" | "full" (default: "full" in dev, "errors" in prod)

	// Peers lists remote ships this ship knows about. At startup each one
	// is discovered, its agent card cached, and tools registered as
	// RemoteTools in the local ToolRegistry.
	Peers []A2APeerConfig

	// CallbackHandler is called when a peer sends a push notification via
	// POST /a2a/callback. Nil = callbacks silently dropped.
	CallbackHandler func(ctx context.Context, peer, event string, payload json.RawMessage) `yaml:"-" json:"-"`

	// TaskTrigger carries a peer task ID from the callback handler to the
	// scheduler. The scheduler wakes the paused task and runs immediately.
	// Empty string = just trigger a run without waking.
	TaskTrigger chan string `yaml:"-" json:"-"`
}

// A2APeerConfig describes a known remote ship.
type A2APeerConfig struct {
	Name      string // stable identifier of the peer agent
	BaseURL   string // e.g. "http://localhost:8090"
	AuthToken string // bearer token to send on outgoing calls
}

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

	// BargeIn, when true, runs the WebSocket voice transport with a
	// concurrent read loop + turn manager so the user can interrupt a
	// response mid-stream (speech_start / cancel frames). When false the
	// transport uses the legacy strictly-sequential read loop.
	// Default false; opt-in, paired with the client-side AEC work.
	BargeIn bool

	// TurnCompletedHook fires after the gateway successfully sends an
	// assistant reply to the user (across Telegram batch, Telegram
	// streaming, voice streaming, and WebSocket batch transports). The
	// host (e.g. Arlene's Layer 2 sleep-time agent) uses this signal to
	// drive per-user state machines without polling. The hook runs in a
	// goroutine inside the gateway, so a slow callback doesn't add
	// latency to the response path. Nil = no-op.
	TurnCompletedHook func(ctx context.Context, userID, sessionID uuid.UUID) `yaml:"-" json:"-"`

	// AgentIterationCompletedHook is the agent_task analogue of
	// TurnCompletedHook: it fires after every successful (non-error)
	// iteration of an agent_task, regardless of strategy or terminal
	// state (Pause / Done / continue). Hosts use this to attach a
	// write-time pipeline (e.g. Arlene's AgentSaver) so research
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
	// (user, soul). The gateway calls it on every inbound Telegram update
	// AFTER pairing interception. Host-implemented (typically a
	// vaelum.bot_links lookup); blueship stays generic.
	//
	// Return ErrTelegramChatUnpaired (or any error whose Is-chain reaches
	// it) to indicate "no link" — the gateway then runs the unpaired-chat
	// policy (greet+signup on platform bots, ignore on user bots). Other
	// errors are treated as transient and the message is dropped with a
	// warning log.
	ResolveTelegramChat func(ctx context.Context, botID uuid.UUID, tgChatID int64) (userID, soulID uuid.UUID, err error) `yaml:"-" json:"-"`
}

// ErrTelegramChatUnpaired signals "this Telegram chat is not linked to any
// Vaelum user on this bot". Hosts return it (or wrap it) from
// GatewayConfig.ResolveTelegramChat so the gateway can run the
// unpaired-chat policy instead of treating the lookup as a transient
// failure.
var ErrTelegramChatUnpaired = errors.New("blueship: telegram chat not paired")

// applyDefaults fills in zero values with sensible defaults.
func (c *Config) ApplyDefaults() {
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	if len(c.SystemPromptKeys) == 0 {
		c.SystemPromptKeys = []string{"preamble", "soul", "agents"}
	}

	// Models
	if c.Models.Primary.Name == "" {
		c.Models.Primary = ModelRef{Provider: "anthropic", Name: "claude-haiku-4-5-20251001"}
	}
	if c.Models.Compact.Name == "" {
		c.Models.Compact = ModelRef{Provider: "anthropic", Name: "claude-haiku-4-5-20251001"}
	}

	// Limits
	if c.Limits.MaxContext == 0 {
		c.Limits.MaxContext = 180000
	}
	if c.Limits.CompactThreshold == 0 {
		c.Limits.CompactThreshold = 40000
	}
	if c.Limits.CompactKeep == 0 {
		c.Limits.CompactKeep = 30000
	}
	if c.Limits.MaxOutputTokens == 0 {
		c.Limits.MaxOutputTokens = 8192
	}
	if c.Limits.CompactOutput == 0 {
		c.Limits.CompactOutput = 2048
	}
	if c.Limits.MinMessageBudget == 0 {
		c.Limits.MinMessageBudget = 10000
	}

	// Timeouts
	if c.Timeouts.LLM == 0 {
		c.Timeouts.LLM = 120 * time.Second
	}
	if c.Timeouts.Compact == 0 {
		c.Timeouts.Compact = 30 * time.Second
	}
	if c.Timeouts.Embedding == 0 {
		c.Timeouts.Embedding = 15 * time.Second
	}
	if c.Timeouts.Transcription == 0 {
		c.Timeouts.Transcription = 30 * time.Second
	}
	if c.Timeouts.TelegramClient == 0 {
		c.Timeouts.TelegramClient = 10 * time.Second
	}
	if c.Timeouts.TelegramPoll == 0 {
		c.Timeouts.TelegramPoll = 35 * time.Second
	}

	// Retry
	if c.Retry.MaxAttempts == 0 {
		c.Retry.MaxAttempts = 3
	}
	if len(c.Retry.Backoff) == 0 {
		c.Retry.Backoff = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}
	}

	// Gateway
	if c.Gateway.DebounceWindow == 0 {
		c.Gateway.DebounceWindow = 1500 * time.Millisecond
	}
	if c.Gateway.DebounceCap == 0 {
		c.Gateway.DebounceCap = 10
	}
	if c.Gateway.MaxTurns == 0 {
		c.Gateway.MaxTurns = 15
	}

	// Telegram multi-bot
	if c.Transport.Telegram.ReloadInterval == 0 {
		c.Transport.Telegram.ReloadInterval = 60 * time.Second
	}
}
