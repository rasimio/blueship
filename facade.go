package blueship

import (
	"github.com/rasimio/blueship/internal/core"
)

// This file is blueship's public facade over the internal core package.
//
// The framework's canonical types, constructors, and helpers live in
// internal/core — invisible to importing applications by Go's internal/
// rule. Everything a host needs is re-exported here so the entire public
// API is reachable as blueship.X, with no second importable package.
//
// When you add an exported symbol to internal/core that hosts must reach,
// add its re-export here. Types use aliases (identical type identity across
// the module boundary); funcs/constructors use value re-exports; consts and
// vars are re-declared.

// --- S0 transport: how users reach a ship ---
type (
	InboundMessage  = core.InboundMessage
	ResponseSink    = core.ResponseSink
	TransportConfig = core.TransportConfig
	TransportSender = core.TransportSender
	MessageSender   = core.MessageSender
	TelegramConfig  = core.TelegramConfig
	BotConfig       = core.BotConfig
	WebSocketConfig = core.WebSocketConfig
	HTTPChatConfig  = core.HTTPChatConfig
)

// --- S1 reflex: fast-tier classification + rules ---
type (
	ReflexContext = core.ReflexContext
	ReflexResult  = core.ReflexResult
	RuleContext   = core.RuleContext
	ActiveRule    = core.ActiveRule
	CandidateRule = core.CandidateRule
	ToolAction    = core.ToolAction
)

// --- S2 cortex: the agent turn + tools + agent tasks ---
type (
	Message         = core.Message
	ContentBlock    = core.ContentBlock
	ToolDefinition  = core.ToolDefinition
	ToolHandler     = core.ToolHandler
	ToolRegistry    = core.ToolRegistry
	Usage           = core.Usage
	AgentHandler    = core.AgentHandler
	AgentTask       = core.AgentTask
	AgentDeps       = core.AgentDeps
	IterationResult = core.IterationResult
)

// --- Providers: LLM + capability ports ---
type (
	CompletionProvider    = core.CompletionProvider
	CompletionRequest     = core.CompletionRequest
	CompletionResponse    = core.CompletionResponse
	EmbeddingProvider     = core.EmbeddingProvider
	SearchEngine          = core.SearchEngine
	SearchResult          = core.SearchResult
	WebFetcher            = core.WebFetcher
	CalendarProvider      = core.CalendarProvider
	CalendarEvent         = core.CalendarEvent
	TranscriptionProvider = core.TranscriptionProvider
	TTSProvider           = core.TTSProvider
	ModelRef              = core.ModelRef
)

// --- Config tree ---
type (
	Config          = core.Config
	ModelsConfig    = core.ModelsConfig
	LimitsConfig    = core.LimitsConfig
	TimeoutsConfig  = core.TimeoutsConfig
	RetryConfig     = core.RetryConfig
	GatewayConfig   = core.GatewayConfig
	OwnerConfig     = core.OwnerConfig
	ToolMeta        = core.ToolMeta
	A2AConfig       = core.A2AConfig
	A2APeerConfig   = core.A2APeerConfig
	FleetConfig     = core.FleetConfig
	FleetCapability = core.FleetCapability
)

// --- Memory / DI / host seams ---
type (
	Deps                  = core.Deps
	Response              = core.Response
	UserProfile           = core.UserProfile
	UserStore             = core.UserStore
	PromptStore           = core.PromptStore
	ModelConfigStore      = core.ModelConfigStore
	SessionQuerier        = core.SessionQuerier
	SessionMessage        = core.SessionMessage
	AttachmentSink        = core.AttachmentSink
	AttachmentRecord      = core.AttachmentRecord
	AttachmentParams      = core.AttachmentParams
	LinkParams            = core.LinkParams
	BotOnboarding         = core.BotOnboarding
	BotOnboardingAccount  = core.BotOnboardingAccount
	BotOnboardingComplete = core.BotOnboardingComplete
)

// --- Constructors & helpers (value re-exports preserve signatures) ---
var (
	NewToolRegistry     = core.NewToolRegistry
	NewModelConfigStore = core.NewModelConfigStore
	NewFilePromptStore  = core.NewFilePromptStore
	NewUserStore        = core.NewUserStore
	NewLLMRouter        = core.NewLLMRouter
	InitDeps            = core.InitDeps

	NormalizeContent = core.NormalizeContent
	EstimateTokens   = core.EstimateTokens
	ExtractText      = core.ExtractText

	WithSoulID          = core.WithSoulID
	SoulIDFromContext   = core.SoulIDFromContext
	SoulIDFromContextOK = core.SoulIDFromContextOK

	OK   = core.OK
	Fail = core.Fail
)

// --- Strategy constants (agent_task dispatch) ---
const (
	StrategyDirect     = core.StrategyDirect
	StrategyStructured = core.StrategyStructured
	StrategyDelegate   = core.StrategyDelegate
)

// --- Sentinel errors ---
var (
	ErrTelegramChatUnpaired     = core.ErrTelegramChatUnpaired
	ErrBotOnboardingAlreadyDone = core.ErrBotOnboardingAlreadyDone
)
