// Package agent runs the S2 cortex turn: the LLM tool loop that drives a
// conversation to completion, handling compaction, tool dispatch, streaming,
// and token budgeting.
package agent

import (
	"context"
	"encoding/json"
	"log/slog"

	bs "github.com/rasimio/blueship/internal/core"
)

// Loop orchestrates the send → tool_use → dispatch → loop cycle.
type Loop struct {
	provider  bs.CompletionProvider
	store     bs.MessageStore
	registry  *bs.ToolRegistry
	roleTools *bs.RoleToolStore // nil = all tools
	compactor *Compactor        // nil = disabled
	logger    *slog.Logger
	cfg       *bs.Config
}

// RunConfig controls agent loop execution.
type RunConfig struct {
	SessionID      string
	SystemPrompt   string
	CompactSummary string // existing compaction summary from previous runs
	Model          string
	MaxTokens      int
	MaxTurns       int
	// ReplyToMessageID, when non-empty, is stamped on the user
	// message row at append time so the cabinet's history endpoint
	// can render a relational reply-quote chip pointing at the
	// parent. Empty for non-reply turns.
	ReplyToMessageID string
	// TGMessageID is the Telegram-side id of this inbound user
	// message. Stamped on the row so a future Telegram reply
	// targeting it can be resolved into our chat_messages.id via
	// session.Store.LookupByTGMessageID. 0 = not from Telegram.
	TGMessageID int64
	// InjectedContext is prepended to the first user turn (not stored in session).
	// Used for automatic memory/context injection before the LLM call.
	InjectedContext string
	// Role selects which tools to send (via RoleToolStore).
	// Empty or unknown role = all tools (backwards-compatible for cloud models).
	Role string
	// ReflexGuidance is a high-priority directive from the reflex phase.
	// Contains expanded matched rules formatted as instructions.
	// Prepended to InjectedContext so it gets maximum attention from the model.
	ReflexGuidance string
	// ToolOverride overrides role-based tool selection with an explicit list.
	// nil = use role default; empty slice = no tools.
	ToolOverride []string
	// AllowedTools is a hard per-soul allowlist applied AFTER role/override
	// selection — the Vaelum cabinet's per-soul tool config. nil = no
	// filtering. A tool absent from this list is dropped even if a role or
	// ToolOverride selected it.
	AllowedTools []string
	// Temperature for LLM generation (0 = provider default).
	Temperature float64
	// Ephemeral, when true, runs the loop without persisting the assistant
	// response or tool results to the session — the user message is still
	// appended unless SkipUserAppend is also set. Used for the interaction
	// tier's escalation pass, whose filler speech is conversational glue
	// rather than a canonical turn message.
	Ephemeral bool
	// SkipUserAppend, when true, skips appending userMessage at loop start
	// because the caller already persisted it. Used so the background tier
	// can continue a turn the interaction tier already opened.
	SkipUserAppend bool
	// MessageBudget, when > 0, overrides the calculated message-window token
	// budget. The fast interaction tier uses this to limit reflex's input to
	// the last ~15-20 turns — Sonnet on 30 K of session history was the
	// dominant per-turn latency, and routing decisions don't need long memory.
	MessageBudget int
	// ThinkingBudget per-RunConfig override of a.cfg.Limits.ThinkingBudget.
	//   0  = inherit global (default)
	//   -1 = explicitly disabled (forces no thinking even if global > 0)
	//   >0 = explicit budget in tokens
	// Set -1 on latency-critical paths (reflex/voice) — thinking-capable
	// models like gemma4-nothinker burn 400-500 hidden tokens (~6 s on M4
	// Max) per turn when enabled. See chooseThinkingBudget below.
	ThinkingBudget int
	// ThinkingMode / Effort are forwarded verbatim to CompletionRequest.
	// ThinkingMode "adaptive" supersedes ThinkingBudget on Claude 4.6+.
	// Effort maps to output_config.effort. See CompletionRequest docs.
	ThinkingMode string
	Effort       string
}

// NewLoop creates a new agent loop.
func NewLoop(provider bs.CompletionProvider, store bs.MessageStore, registry *bs.ToolRegistry, roleTools *bs.RoleToolStore, cfg *bs.Config, logger *slog.Logger) *Loop {
	return &Loop{
		provider:  provider,
		store:     store,
		registry:  registry,
		roleTools: roleTools,
		cfg:       cfg,
		logger:    logger,
	}
}

// SetCompactor enables conversation compaction.
func (a *Loop) SetCompactor(c *Compactor) {
	a.compactor = c
}

// ToolTrace records a single tool invocation during the agent loop.
type ToolTrace struct {
	Name   string `json:"name"`
	Input  string `json:"input"`
	Output string `json:"output,omitempty"`
	Error  bool   `json:"error,omitempty"`
}

// RunResult extends the text response with tool execution trace.
type RunResult struct {
	Text       string
	ToolTraces []ToolTrace
}

// Run executes the agent loop and returns the final text response.
func (a *Loop) Run(ctx context.Context, cfg RunConfig, userMessage any) (string, error) {
	result, err := a.RunTracked(ctx, cfg, userMessage)
	if err != nil {
		return "", err
	}
	return result.Text, nil
}

// RunTracked executes the agent loop and returns text + tool traces.

func chooseThinkingBudget(cfgValue, globalDefault int) int {
	if cfgValue < 0 {
		return 0
	}
	if cfgValue > 0 {
		return cfgValue
	}
	if globalDefault < 0 {
		return 0
	}
	return globalDefault
}

func (a *Loop) calculateBudget(systemPrompt string, tools []bs.ToolDefinition) int {
	maxContext := a.cfg.Limits.MaxContext

	systemTokens := len([]rune(systemPrompt)) / 3

	toolSchemaTokens := 0
	if len(tools) > 0 {
		data, _ := json.Marshal(tools)
		toolSchemaTokens = len(data) / 3
	}

	minBudget := a.cfg.Limits.MinMessageBudget
	budget := maxContext - systemTokens - toolSchemaTokens
	if budget < minBudget {
		budget = minBudget
	}
	return budget
}
