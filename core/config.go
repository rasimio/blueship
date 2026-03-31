package core

import "time"

// Config controls BlueShip runtime behavior.
// All fields have sensible defaults; only LLM, Transport, and DB are required.
type Config struct {
	// --- Required ---
	LLM       CompletionProvider // e.g. blueship.Anthropic(apiKey)
	Transport TransportConfig    // e.g. blueship.Telegram(botToken)
	DB        string             // PostgreSQL DSN (app database)
	ShipSchema string            // Schema for BlueShip tables (default: "" = public)

	// --- Optional providers (nil = disabled) ---
	Embedder    EmbeddingProvider      // default: nil (embedding features disabled)
	Search      SearchEngine           // default: nil (web_search tool disabled)
	Fetcher     WebFetcher             // default: nil (auto-created if nil)
	Calendar    CalendarProvider       // default: nil
	Transcriber TranscriptionProvider  // default: nil (voice disabled)
	TTS              TTSProvider                   // default: nil (text-to-speech disabled)
	TTSVoice         string                        // default TTS voice name
	TTSInstructMapper func(strategy string) string    // maps emotion strategy to TTS instruct
	TTSTextCleaner   func(text string) string         // strips kaomoji/markdown for TTS
	TTSConverter     func(wav []byte) ([]byte, error) // WAV→OGG converter (nil = send WAV as-is)
	Sender           MessageSender                 // default: nil (message sending disabled)

	// --- Optional infrastructure ---
	Redis    string // Redis address (default: "" = no cache)
	Prompts  string // directory with .md prompt files (default: "" = no files)
	Timezone string // default: "UTC"

	// SystemPromptKeys defines prompt keys that compose the system prompt.
	// Loaded from DB (system_prompts) or workspace files (KEY.md).
	// Default: ["preamble", "soul", "agents"]
	SystemPromptKeys []string

	// --- Owner (single-user mode) ---
	Owner OwnerConfig

	// --- Fine-tuning (all have defaults) ---
	Models   ModelsConfig
	Limits   LimitsConfig
	Timeouts TimeoutsConfig
	Retry    RetryConfig
	Gateway  GatewayConfig
}

// OwnerConfig identifies the single owner of this instance.
type OwnerConfig struct {
	ChatID      string // e.g. "telegram:5452235517"
	DisplayName string // e.g. "Alice"
}

// TransportConfig holds transport configuration.
type TransportConfig struct {
	Type     string // "telegram"
	BotToken string
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
	MaxContext        int // Opus input budget (default: 180000)
	CompactThreshold  int // trigger compaction above this (default: 40000)
	CompactKeep       int // keep recent messages intact (default: 30000)
	MaxOutputTokens   int // agent loop max output (default: 8192)
	CompactOutput     int // haiku compaction output (default: 2048)
	ThinkingBudget    int // extended thinking budget (default: 0 = disabled)
	MinMessageBudget  int // minimum token budget for messages (default: 10000)
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

// GatewayConfig defines gateway behavior.
type GatewayConfig struct {
	DebounceWindow   time.Duration // default: 1500ms
	DebounceCap      int           // default: 10
	SessionResetHour int           // default: 4 (4am)
	MaxTurns         int           // default: 15
	Debug bool // send errors to owner via Telegram instead of "Sorry..."
}

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
	if c.Gateway.SessionResetHour == 0 {
		c.Gateway.SessionResetHour = 4
	}
	if c.Gateway.MaxTurns == 0 {
		c.Gateway.MaxTurns = 15
	}
}
