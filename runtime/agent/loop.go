// Package agent runs the S2 cortex turn: the LLM tool loop that drives a
// conversation to completion, handling compaction, tool dispatch, streaming,
// and token budgeting.
package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

// Loop orchestrates the send → tool_use → dispatch → loop cycle.
type Loop struct {
	provider  bs.CompletionProvider
	store     bs.MessageStore
	registry  *bs.ToolRegistry
	roleTools bs.RoleToolQuerier // nil = all tools
	compactor *Compactor         // nil = disabled
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
	ContextWindow  int
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
	// InjectedContext is added to the per-run turn context (not stored in session).
	// Used for automatic memory/context injection before the LLM call without
	// consuming the visible dialogue message budget.
	InjectedContext string
	// Role selects which tools to send (via RoleToolStore).
	// Empty or unknown role = all tools (backwards-compatible for cloud models).
	Role string
	// ReflexGuidance is a high-priority directive from the reflex phase.
	// Contains expanded matched rules formatted as instructions.
	// Prepended to InjectedContext inside the turn context so it gets maximum
	// attention from the model without consuming visible dialogue budget.
	ReflexGuidance string
	// ToolOverride overrides role-based tool selection with an explicit list.
	// nil = use role default; empty slice = no tools.
	ToolOverride []string
	// ToolSelectionSource is diagnostic text describing why ToolOverride was
	// chosen. It is logged only; empty means role/default selection.
	ToolSelectionSource string
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
	// MessageBudget, when > 0, overrides the default message-window token
	// budget.
	MessageBudget int
	// MessageBudgetSource explains where MessageBudget came from. It is stored
	// in llm_usage so prompt-budget regressions can be diagnosed from data.
	MessageBudgetSource string
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
	// OnTiming receives per-component latency spans for observability. It must
	// not affect loop behavior; callers may leave it nil.
	OnTiming func(bs.TimingSpan)
	// ToolTimeout caps a single tool execution. Zero uses per-tool defaults.
	ToolTimeout time.Duration
}

// NewLoop creates a new agent loop.
func NewLoop(provider bs.CompletionProvider, store bs.MessageStore, registry *bs.ToolRegistry, roleTools bs.RoleToolQuerier, cfg *bs.Config, logger *slog.Logger) *Loop {
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

func (a *Loop) effectiveMessageBudget(cfg RunConfig, systemPrompt string, tools []bs.ToolDefinition) bs.MessageBudgetDecision {
	return bs.ResolveMessageBudget(bs.MessageBudgetRequest{
		Role:           cfg.Role,
		ExplicitBudget: cfg.MessageBudget,
		ExplicitSource: cfg.MessageBudgetSource,
		Config:         a.cfg,
		SystemPrompt:   systemPrompt,
		Tools:          tools,
	})
}

func (a *Loop) selectTools(cfg RunConfig) []bs.ToolDefinition {
	var tools []bs.ToolDefinition
	if cfg.ToolOverride != nil {
		tools = a.registry.DefinitionsForNames(cfg.ToolOverride)
	} else if cfg.Role != "" && a.roleTools != nil {
		if names := a.roleTools.Get(cfg.Role); names != nil {
			tools = a.registry.DefinitionsForNames(names)
		} else {
			tools = a.registry.Definitions()
		}
	} else {
		tools = a.registry.Definitions()
	}
	if cfg.AllowedTools == nil {
		return tools
	}
	allow := make(map[string]bool, len(cfg.AllowedTools))
	for _, n := range cfg.AllowedTools {
		allow[n] = true
	}
	kept := make([]bs.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if allow[t.Name] {
			kept = append(kept, t)
		}
	}
	return kept
}

func effectiveSystemPrompt(systemPrompt, compactSummary, turnContext string) string {
	effective := systemPrompt
	if compactSummary != "" {
		effective += SummaryHeader + compactSummary
	}
	if turnContext != "" {
		effective += "\n\n## Turn context\n[turn_context]\n" + turnContext + "\n[/turn_context]"
	}
	return effective
}

func buildTurnContextForTools(reflexGuidance, injectedContext string, tools []bs.ToolDefinition, extraContext ...string) string {
	var parts []string
	if s := strings.TrimSpace(reflexGuidance); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(injectedContext); s != "" {
		parts = append(parts, s)
	}
	for _, value := range extraContext {
		if s := strings.TrimSpace(value); s != "" {
			parts = append(parts, s)
		}
	}
	parts = append(parts, formatAvailableToolsContext(tools))
	return strings.Join(parts, "\n\n")
}

func formatAvailableToolsContext(tools []bs.ToolDefinition) string {
	var b strings.Builder
	b.WriteString("[available_tools]\n")
	if len(tools) == 0 {
		b.WriteString("none. No native tool_use calls are available in this turn. Do not claim you can call tools; answer from visible context or say what is missing.\n")
		b.WriteString("[/available_tools]")
		return b.String()
	}
	b.WriteString("Only these native tool_use calls are available in this turn:\n")
	for _, tool := range tools {
		if tool.Name == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(tool.Name)
		b.WriteByte('\n')
	}
	b.WriteString("[/available_tools]")
	return b.String()
}

func cloneMessages(messages []bs.Message) []bs.Message {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]bs.Message, len(messages))
	copy(cloned, messages)
	return cloned
}

func estimateTextTokens(s string) int {
	return bs.EstimateTextTokens(s)
}

func estimateToolSchemaTokens(tools []bs.ToolDefinition) int {
	if len(tools) == 0 {
		return 0
	}
	data, _ := json.Marshal(tools)
	return bs.EstimateTextTokens(string(data))
}

const (
	dialogBudgetModeTotalPrompt    = "total_prompt"
	dialogBudgetModeDialogFallback = "dialog_budget_overhead_exceeded"
	dialogBudgetModeUnbounded      = "unbounded"
)

type dialogBudgetDecision struct {
	DialogBudget                int
	PromptOverhead              int
	Mode                        string
	PromptOverheadExceedsBudget bool
}

func effectiveDialogBudget(totalPromptBudget int, systemPrompt, compactSummary, turnContext string, tools []bs.ToolDefinition) (dialogBudget int, promptOverhead int) {
	decision := effectiveDialogBudgetDecision(totalPromptBudget, systemPrompt, compactSummary, turnContext, tools)
	return decision.DialogBudget, decision.PromptOverhead
}

func effectiveDialogBudgetDecision(totalPromptBudget int, systemPrompt, compactSummary, turnContext string, tools []bs.ToolDefinition) dialogBudgetDecision {
	if totalPromptBudget <= 0 {
		return dialogBudgetDecision{DialogBudget: totalPromptBudget, Mode: dialogBudgetModeUnbounded}
	}
	promptOverhead := estimateTextTokens(effectiveSystemPrompt(systemPrompt, compactSummary, turnContext)) + estimateToolSchemaTokens(tools)
	if promptOverhead >= totalPromptBudget {
		return dialogBudgetDecision{
			DialogBudget:                totalPromptBudget,
			PromptOverhead:              promptOverhead,
			Mode:                        dialogBudgetModeDialogFallback,
			PromptOverheadExceedsBudget: true,
		}
	}
	dialogBudget := totalPromptBudget - promptOverhead
	if dialogBudget < 1 {
		dialogBudget = 1
	}
	return dialogBudgetDecision{
		DialogBudget:   dialogBudget,
		PromptOverhead: promptOverhead,
		Mode:           dialogBudgetModeTotalPrompt,
	}
}

func maxToolTurnsForRole(role string) int {
	switch role {
	case "cortex":
		return 3
	case "background":
		return 8
	default:
		return 5
	}
}

func appendFinalAnswerDirective(msg *bs.Message) {
	if msg == nil || msg.Role != "user" {
		return
	}
	blocks := bs.NormalizeContent(msg.Content)
	directive := bs.ContentBlock{Type: "text", Text: "\n\n[tool_limit]\nNo more tools are available for this turn. Use the tool results already provided and answer the user's request now. Do not ask to run another search.\n[/tool_limit]"}
	msg.Content = append(blocks, directive)
}

const (
	maxTokenContinuationPasses = 1
	maxTokenOverlapRunes       = 800
)

func maxTokenContinuationMessage() bs.Message {
	return bs.Message{
		Role: "user",
		Content: []bs.ContentBlock{{
			Type: "text",
			Text: "[max_tokens_continuation]\nYour previous answer was cut off by the output token limit. Continue exactly from the last visible text. Do not restart, summarize, apologize, or repeat earlier content. Output only the continuation.\n[/max_tokens_continuation]",
		}},
	}
}

func hasVisibleOutput(content []bs.ContentBlock) bool {
	for _, block := range content {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				return true
			}
		case "tool_use":
			return true
		}
	}
	return false
}

func hasToolUseOutput(content []bs.ContentBlock) bool {
	for _, block := range content {
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

func shouldAutoContinueMaxTokens(resp *bs.CompletionResponse, continuationPasses int) bool {
	return resp != nil &&
		resp.StopReason == "max_tokens" &&
		continuationPasses < maxTokenContinuationPasses &&
		strings.TrimSpace(bs.ExtractText(resp.Content)) != "" &&
		!hasToolUseOutput(resp.Content)
}

func mergeContinuationText(prefix, continuation string) string {
	prefix = strings.TrimRight(prefix, " \t\r\n")
	continuation = strings.TrimLeft(continuation, " \t\r\n")
	if prefix == "" {
		return continuation
	}
	if continuation == "" {
		return prefix
	}

	prefixRunes := []rune(prefix)
	continuationRunes := []rune(continuation)
	maxOverlap := len(prefixRunes)
	if len(continuationRunes) < maxOverlap {
		maxOverlap = len(continuationRunes)
	}
	if maxOverlap > maxTokenOverlapRunes {
		maxOverlap = maxTokenOverlapRunes
	}
	for n := maxOverlap; n >= 20; n-- {
		if strings.HasSuffix(prefix, string(continuationRunes[:n])) {
			continuation = string(continuationRunes[n:])
			break
		}
	}
	if strings.TrimSpace(continuation) == "" {
		return prefix
	}
	return prefix + "\n\n" + strings.TrimLeft(continuation, " \t\r\n")
}

func shouldRetryEmptyVisibleOutput(resp *bs.CompletionResponse, req bs.CompletionRequest) bool {
	if resp == nil || resp.StopReason != "max_tokens" || hasVisibleOutput(resp.Content) {
		return false
	}
	return req.Effort != "" || req.ThinkingMode != "" || req.ThinkingBudget > 0
}

func withoutReasoning(req bs.CompletionRequest) bs.CompletionRequest {
	req.Effort = ""
	req.ThinkingMode = "off"
	req.ThinkingBudget = 0
	return req
}

func (a *Loop) emptyVisibleFallback() string {
	if a != nil && a.cfg != nil && strings.TrimSpace(a.cfg.UI.ModelRefused) != "" {
		return a.cfg.UI.ModelRefused
	}
	return "(the model produced no visible answer — retry with a shorter request)"
}
