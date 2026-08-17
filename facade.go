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
	Message                       = core.Message
	PersistedMessage              = core.PersistedMessage
	PersistedMessageAppender      = core.PersistedMessageAppender
	PersistedMessageTokenAppender = core.PersistedMessageTokenAppender
	MessageProjection             = core.MessageProjection
	MessageProjectionStatus       = core.MessageProjectionStatus
	ContentBlock                  = core.ContentBlock
	ImageSource                   = core.ImageSource
	ToolDefinition                = core.ToolDefinition
	ToolHandler                   = core.ToolHandler
	ToolRegistry                  = core.ToolRegistry
	ToolSelector                  = core.ToolSelector
	ToolSelection                 = core.ToolSelection
	ToolSelectionRequest          = core.ToolSelectionRequest
	TurnPolicyMode                = core.TurnPolicyMode
	TurnPolicy                    = core.TurnPolicy
	TurnPolicyRequest             = core.TurnPolicyRequest
	TurnPolicyResolver            = core.TurnPolicyResolver
	Usage                         = core.Usage
	AgentHandler                  = core.AgentHandler
	AgentTask                     = core.AgentTask
	AgentDeps                     = core.AgentDeps
	IterationResult               = core.IterationResult
	TaskProgram                   = core.TaskProgram
	TaskProgramInput              = core.TaskProgramInput
	TaskProgramActivation         = core.TaskProgramActivation
	TaskProgramDecision           = core.TaskProgramDecision
	TaskProgramQuietHours         = core.TaskProgramQuietHours
	TaskDeliveryRef               = core.TaskDeliveryRef
	SkillMeta                     = core.SkillMeta
	ExecutionKind                 = core.ExecutionKind
	ExecutionRequest              = core.ExecutionRequest
	ExecutionDecision             = core.ExecutionDecision
	DecisionAction                = core.DecisionAction
	ExecutionAuthorizer           = core.ExecutionAuthorizer
	ImageGenerator                = core.ImageGenerator
	ImageResult                   = core.ImageResult
	AutonomousTurnRequest         = core.AutonomousTurnRequest
	AutonomousTurnDraft           = core.AutonomousTurnDraft
	AutonomousTurnDrafter         = core.AutonomousTurnDrafter
	AutonomousTurnCommit          = core.AutonomousTurnCommit
)

// --- Providers: LLM + capability ports ---
type (
	CompletionProvider       = core.CompletionProvider
	CompletionRequest        = core.CompletionRequest
	CompletionResponse       = core.CompletionResponse
	StreamCompletionProvider = core.StreamCompletionProvider
	StreamCallbacks          = core.StreamCallbacks

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
	Config               = core.Config
	ModelsConfig         = core.ModelsConfig
	LimitsConfig         = core.LimitsConfig
	TimeoutsConfig       = core.TimeoutsConfig
	RetryConfig          = core.RetryConfig
	GatewayConfig        = core.GatewayConfig
	UIStrings            = core.UIStrings
	OnboardingMessages   = core.OnboardingMessages
	OnboardingVoice      = core.OnboardingVoice
	OnboardingTrait      = core.OnboardingTrait
	OnboardingFlow       = core.OnboardingFlow
	OnboardingMode       = core.OnboardingMode
	OnboardingSeedButton = core.OnboardingSeedButton
	BotCommand           = core.BotCommand
	BotCommandRequest    = core.BotCommandRequest
	BotCommandResult     = core.BotCommandResult
	BotCommandHandler    = core.BotCommandHandler
	BotCommandButton     = core.BotCommandButton
	InvoiceOffer         = core.InvoiceOffer
	BotMenu              = core.BotMenu
	BotMenuNode          = core.BotMenuNode
	BotMenuItem          = core.BotMenuItem
	BotKeyboard          = core.BotKeyboard
	BotKeyboardButton    = core.BotKeyboardButton

	// In-chat payments: the host decides what a purchase means, the
	// transport carries it.
	PreCheckout           = core.PreCheckout
	PaymentReceipt        = core.PaymentReceipt
	ApprovePaymentFunc    = core.ApprovePaymentFunc
	PaymentReceivedFunc   = core.PaymentReceivedFunc
	BotPersonaEditor      = core.BotPersonaEditor
	BotPersonaUpdate      = core.BotPersonaUpdate
	OwnerConfig           = core.OwnerConfig
	ToolMeta              = core.ToolMeta
	A2AConfig             = core.A2AConfig
	A2APeerConfig         = core.A2APeerConfig
	FleetConfig           = core.FleetConfig
	FleetCapability       = core.FleetCapability
	MetricType            = core.MetricType
	MetricSample          = core.MetricSample
	MetricSampleCollector = core.MetricSampleCollector
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
	WithAttachmentIntents            = core.WithAttachmentIntents
	RecordAttachmentIntent           = core.RecordAttachmentIntent
	EnsureAttachmentMarkers          = core.EnsureAttachmentMarkers
	NewToolRegistry                  = core.NewToolRegistry
	RoleToolKey                      = core.RoleToolKey
	NewModelConfigStore              = core.NewModelConfigStore
	NewFilePromptStore               = core.NewFilePromptStore
	NewUserStore                     = core.NewUserStore
	NewLLMRouter                     = core.NewLLMRouter
	InitDeps                         = core.InitDeps
	ParseTaskProgram                 = core.ParseTaskProgram
	FormatAutonomousTurnNotification = core.FormatAutonomousTurnNotification
	ParseAutonomousTurnNotification  = core.ParseAutonomousTurnNotification

	NormalizeContent     = core.NormalizeContent
	EstimateTokens       = core.EstimateTokens
	EstimateTextTokens   = core.EstimateTextTokens
	ExtractText          = core.ExtractText
	ProjectLegacyMessage = core.ProjectLegacyMessage

	WithSoulID                = core.WithSoulID
	SoulIDFromContext         = core.SoulIDFromContext
	SoulIDFromContextOK       = core.SoulIDFromContextOK
	WithUserID                = core.WithUserID
	UserIDFromContext         = core.UserIDFromContext
	UserIDFromContextOK       = core.UserIDFromContextOK
	ContextWithAutonomousTurn = core.ContextWithAutonomousTurn
	IsAutonomousTurn          = core.IsAutonomousTurn
	WithDeniedTools           = core.WithDeniedTools
	DeniedToolsFromContext    = core.DeniedToolsFromContext
	IsToolDenied              = core.IsToolDenied

	OK   = core.OK
	Fail = core.Fail
)

const (
	TurnPolicyOff    = core.TurnPolicyOff
	TurnPolicyShadow = core.TurnPolicyShadow
	TurnPolicyCanary = core.TurnPolicyCanary
	TurnPolicyOn     = core.TurnPolicyOn
)

const (
	MetricGauge   = core.MetricGauge
	MetricCounter = core.MetricCounter
)

// --- Why an autonomous turn produced no message ---
//
// Hosts distinguish these to decide whether asking again is sensible. Only
// ModelDeclined reflects a choice; the rest report facts about the
// conversation that a second call would restate rather than change — and
// retrying UserActive in particular would interrupt someone mid-sentence.
const (
	AutonomousNoOpUserActive    = core.AutonomousNoOpUserActive
	AutonomousNoOpNoDialog      = core.AutonomousNoOpNoDialog
	AutonomousNoOpRuleSilent    = core.AutonomousNoOpRuleSilent
	AutonomousNoOpModelDeclined = core.AutonomousNoOpModelDeclined
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

const (
	OnboardingModeWizard  = core.OnboardingModeWizard
	OnboardingModeInstant = core.OnboardingModeInstant
)

const (
	ProjectionProjected              = core.ProjectionProjected
	ProjectionNonDialogue            = core.ProjectionNonDialogue
	ProjectionUnprojectableLegacy    = core.ProjectionUnprojectableLegacy
	CanonicalMessageProjectorVersion = core.CanonicalMessageProjectorVersion
	LegacyMessageProjectorVersion    = core.LegacyMessageProjectorVersion
)

// --- Sentinel errors ---
var (
	ErrTelegramChatUnpaired     = core.ErrTelegramChatUnpaired
	ErrBotOnboardingAlreadyDone = core.ErrBotOnboardingAlreadyDone
	ErrBotPersonaNoSoul         = core.ErrBotPersonaNoSoul
	ErrExecutionDenied          = core.ErrExecutionDenied
	ErrPaymentUnavailable       = core.ErrPaymentUnavailable
)
