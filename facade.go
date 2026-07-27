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
	ContextInfoSink = core.ContextInfoSink
	ContextInfo     = core.ContextInfo
	MatchedRule     = core.MatchedRule
	TimingSink      = core.TimingSink
	TimingReport    = core.TimingReport
	TimingSpan      = core.TimingSpan
)

// --- S1 reflex: fast-tier classification + rules ---
type (
	ReflexContext           = core.ReflexContext
	ReflexResult            = core.ReflexResult
	RuleContext             = core.RuleContext
	ActiveRule              = core.ActiveRule
	CandidateRule           = core.CandidateRule
	ToolAction              = core.ToolAction
	ReflexPreActionRequest  = core.ReflexPreActionRequest
	ReflexPreActionSelector = core.ReflexPreActionSelector
)

// --- S2 cortex: the agent turn + tools + agent tasks ---
type (
	Message               = core.Message
	ContentBlock          = core.ContentBlock
	ToolDefinition        = core.ToolDefinition
	ToolHandler           = core.ToolHandler
	ToolRegistry          = core.ToolRegistry
	ToolSelector          = core.ToolSelector
	ToolSelection         = core.ToolSelection
	ToolSelectionRequest  = core.ToolSelectionRequest
	Usage                 = core.Usage
	AgentHandler          = core.AgentHandler
	AgentTask             = core.AgentTask
	AgentDeps             = core.AgentDeps
	IterationResult       = core.IterationResult
	TaskProgram           = core.TaskProgram
	TaskProgramInput      = core.TaskProgramInput
	TaskProgramActivation = core.TaskProgramActivation
	TaskProgramDecision   = core.TaskProgramDecision
	TaskProgramQuietHours = core.TaskProgramQuietHours
	TaskDeliveryRef       = core.TaskDeliveryRef
	SkillMeta             = core.SkillMeta
	ExecutionKind         = core.ExecutionKind
	ExecutionRequest      = core.ExecutionRequest
	ExecutionDecision     = core.ExecutionDecision
	ExecutionAuthorizer   = core.ExecutionAuthorizer
	AutonomousTurnRequest = core.AutonomousTurnRequest
	AutonomousTurnDraft   = core.AutonomousTurnDraft
	AutonomousTurnDrafter = core.AutonomousTurnDrafter
	AutonomousTurnCommit  = core.AutonomousTurnCommit
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
	Config             = core.Config
	ModelsConfig       = core.ModelsConfig
	LimitsConfig       = core.LimitsConfig
	TimeoutsConfig     = core.TimeoutsConfig
	RetryConfig        = core.RetryConfig
	GatewayConfig      = core.GatewayConfig
	UIStrings          = core.UIStrings
	OnboardingMessages = core.OnboardingMessages
	OwnerConfig        = core.OwnerConfig
	ToolMeta           = core.ToolMeta
	A2AConfig          = core.A2AConfig
	A2APeerConfig      = core.A2APeerConfig
	FleetConfig        = core.FleetConfig
	FleetCapability    = core.FleetCapability
)

// --- Memory / DI / host seams ---
type (
	Deps                  = core.Deps
	Response              = core.Response
	UserProfile           = core.UserProfile
	UserStore             = core.UserStore
	LLMUsageRecord        = core.LLMUsageRecord
	LLMUsageRecorder      = core.LLMUsageRecorder
	PromptStore           = core.PromptStore
	ModelConfigStore      = core.ModelConfigStore
	ModelConfigQuerier    = core.ModelConfigQuerier
	RoleToolQuerier       = core.RoleToolQuerier
	SessionQuerier        = core.SessionQuerier
	SessionMessage        = core.SessionMessage
	AttachmentSink        = core.AttachmentSink
	AttachmentRecord      = core.AttachmentRecord
	AttachmentParams      = core.AttachmentParams
	LinkParams            = core.LinkParams
	BotOnboarding         = core.BotOnboarding
	DeeplinkLoginApprover = core.DeeplinkLoginApprover
	DeeplinkLinker        = core.DeeplinkLinker
	BotOnboardingAccount  = core.BotOnboardingAccount
	BotOnboardingComplete = core.BotOnboardingComplete
)

// --- Constructors & helpers (value re-exports preserve signatures) ---
var (
	NewToolRegistry                  = core.NewToolRegistry
	NewModelConfigStore              = core.NewModelConfigStore
	NewFilePromptStore               = core.NewFilePromptStore
	NewUserStore                     = core.NewUserStore
	NewLLMRouter                     = core.NewLLMRouter
	InitDeps                         = core.InitDeps
	ParseTaskProgram                 = core.ParseTaskProgram
	FormatAutonomousTurnNotification = core.FormatAutonomousTurnNotification
	ParseAutonomousTurnNotification  = core.ParseAutonomousTurnNotification

	NormalizeContent   = core.NormalizeContent
	EstimateTokens     = core.EstimateTokens
	EstimateTextTokens = core.EstimateTextTokens
	ExtractText        = core.ExtractText

	WithSoulID                = core.WithSoulID
	SoulIDFromContext         = core.SoulIDFromContext
	SoulIDFromContextOK       = core.SoulIDFromContextOK
	WithUserID                = core.WithUserID
	UserIDFromContext         = core.UserIDFromContext
	UserIDFromContextOK       = core.UserIDFromContextOK
	ContextWithAutonomousTurn = core.ContextWithAutonomousTurn
	IsAutonomousTurn          = core.IsAutonomousTurn

	OK   = core.OK
	Fail = core.Fail
)

// --- Strategy constants (agent_task dispatch) ---
const (
	StrategyDirect     = core.StrategyDirect
	StrategyStructured = core.StrategyStructured
	StrategyDelegate   = core.StrategyDelegate
)

const (
	TaskProgramSchemaV1                    = core.TaskProgramSchemaV1
	TaskProgramMaxInputs                   = core.TaskProgramMaxInputs
	TaskProgramMaxDecisionTools            = core.TaskProgramMaxDecisionTools
	TaskProgramMaxInputBytes               = core.TaskProgramMaxInputBytes
	TaskProgramMaxDecisionInstructionBytes = core.TaskProgramMaxDecisionInstructionBytes
	TaskProgramOnErrorFail                 = core.TaskProgramOnErrorFail
	TaskProgramOnErrorContinue             = core.TaskProgramOnErrorContinue
	TaskProgramActivationAlways            = core.TaskProgramActivationAlways
	TaskProgramActivationAnyNonEmpty       = core.TaskProgramActivationAnyNonEmpty
	TaskProgramActivationAllNonEmpty       = core.TaskProgramActivationAllNonEmpty
	TaskProgramDecisionSelected            = core.TaskProgramDecisionSelected
	TaskProgramDecisionNone                = core.TaskProgramDecisionNone
)

const (
	ExecutionInteractive = core.ExecutionInteractive
	ExecutionBackground  = core.ExecutionBackground
)

// --- Sentinel errors ---
var (
	ErrTelegramChatUnpaired     = core.ErrTelegramChatUnpaired
	ErrBotOnboardingAlreadyDone = core.ErrBotOnboardingAlreadyDone
	ErrExecutionDenied          = core.ErrExecutionDenied
)
