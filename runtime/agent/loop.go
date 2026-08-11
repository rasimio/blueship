// Package agent runs the S2 cortex turn: the LLM tool loop that drives a
// conversation to completion, handling compaction, tool dispatch, streaming,
// and token budgeting.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
	// TGMessageIDs are the Telegram-side ids of the inbound messages
	// this turn answers — several when the debouncer folded a burst
	// into one turn. Stamped on the row so a future Telegram reply
	// targeting ANY of them resolves into our chat_messages.id via
	// session.Store.LookupByTGMessageID. Empty = not from Telegram.
	TGMessageIDs []int64
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
	// ToolboxExpansion, when non-empty alongside a non-nil ToolOverride,
	// arms the open_toolbox escape hatch: the loop appends a synthetic
	// open_toolbox tool to the turn's tool list, and when the model calls
	// it the active toolset is replaced with this list (resolved through
	// the same role registry + AllowedTools filters as normal selection).
	// This makes a narrowing tool selector fail open on demand — a missed
	// pack costs one extra round-trip instead of the whole turn.
	ToolboxExpansion []string
	// ToolSelectionSource is diagnostic text describing why ToolOverride was
	// chosen. It is logged only; empty means role/default selection.
	ToolSelectionSource string
	// AllowedTools is a hard per-soul allowlist applied AFTER role/override
	// selection — the Vaelum cabinet's per-soul tool config. nil = no
	// filtering. A tool absent from this list is dropped even if a role or
	// ToolOverride selected it.
	AllowedTools []string
	// DeniedTools is a hard per-turn denylist applied after role, override,
	// toolbox, and AllowedTools resolution. It also flows through the tool
	// execution context so live capability reporters hide denied tools.
	DeniedTools []string
	// StrictTools prevents dispatch of any tool outside the concrete tool
	// definitions sent for this turn. It also disables toolbox expansion.
	StrictTools bool
	// TurnPolicyActive bypasses any optional interaction-tier answer pass so
	// mandatory evidence and the resolved Cortex tool contract cannot be
	// silently skipped by a faster model.
	TurnPolicyActive bool
	// VisibleUserText is the exact user-visible transport text/transcript
	// captured before reply-parent and attachment expansion. Nil means the
	// caller could not provide a canonical projection; pointer-to-empty is a
	// valid attachment-only message.
	VisibleUserText *string
	// OnUserMessagePersisted receives the exact durable append receipt after
	// the human row commits. It is used for dependent evidence rows such as
	// inbound attachments; nil keeps legacy/custom stores unchanged.
	OnUserMessagePersisted func(context.Context, bs.PersistedMessage)
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
	// PromptOnlyInput, when true, appends userMessage only to the in-memory
	// conversation sent to the provider. It is never written to MessageStore.
	// Assistant and tool-result persistence still follows Ephemeral.
	PromptOnlyInput bool
	// EmptyVisibleFallback overrides the user-facing text emitted when a
	// provider ends the turn without visible output. Autonomous callers set
	// this to their no-op sentinel so an empty model response stays silent.
	EmptyVisibleFallback string
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
	// TurnNow, when non-zero, is "now" in the user's timezone and turns on
	// temporal annotation of the dialog window: [date: <Weekday YYYY-MM-DD>]
	// markers are inserted where the calendar day changes between stored
	// messages, and a [felt_time] block is added to the turn context when
	// the previous message is stale. Without it a windowed transcript reads
	// as one continuous "today" and the model narrates from the wrong day.
	TurnNow time.Time
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
	Name    string `json:"name"`
	BlockID string `json:"block_id,omitempty"`
	Input   string `json:"input"`
	Output  string `json:"output,omitempty"`
	Error   bool   `json:"error,omitempty"`
}

// RunResult extends the text response with tool execution trace.
type RunResult struct {
	Text       string
	ToolTraces []ToolTrace
}

// durableUserContent returns the transport-visible envelope written to
// chat_messages. Provider-expanded reply parents, attachment bytes, and
// extracted file bodies stay in userMessage for the current call only.
func durableUserContent(cfg RunConfig, userMessage any) any {
	if cfg.VisibleUserText == nil {
		return userMessage
	}
	return *cfg.VisibleUserText
}

func appendPersistedMessage(
	ctx context.Context,
	store bs.MessageStore,
	sessionID string,
	msg bs.Message,
) (bs.PersistedMessage, error) {
	if appender, ok := store.(bs.PersistedMessageAppender); ok {
		return appender.AppendPersisted(ctx, sessionID, msg)
	}
	if err := store.Append(ctx, sessionID, msg); err != nil {
		return bs.PersistedMessage{}, err
	}
	return bs.PersistedMessage{Role: msg.Role}, nil
}

// overlayCurrentUserContent swaps the just-persisted canonical user envelope
// for the provider-expanded payload in this one in-memory prompt. The turn
// lock guarantees that a freshly appended human row is the latest dialogue
// message; if a custom store omits it, append the current payload explicitly.
func overlayCurrentUserContent(messages []bs.Message, cfg RunConfig, userMessage any) []bs.Message {
	if cfg.PromptOnlyInput || cfg.VisibleUserText == nil {
		return messages
	}
	if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
		messages[len(messages)-1].Content = userMessage
		messages[len(messages)-1].VisibleText = cfg.VisibleUserText
		return messages
	}
	return append(messages, bs.Message{
		Role:        "user",
		Content:     userMessage,
		VisibleText: cfg.VisibleUserText,
	})
}

// dialogBudgetAfterCurrentExpansion reserves space for provider-only current
// payload bytes without charging that expansion to durable dialogue history.
func dialogBudgetAfterCurrentExpansion(budget int, cfg RunConfig, userMessage any) int {
	if budget <= 0 || cfg.VisibleUserText == nil || cfg.PromptOnlyInput {
		return budget
	}
	expanded := bs.EstimateTokens(bs.NormalizeContent(userMessage))
	canonical := bs.EstimateTokens(bs.NormalizeContent(*cfg.VisibleUserText))
	extra := expanded - canonical
	if extra <= 0 {
		return budget
	}
	if extra >= budget {
		return 1
	}
	return budget - extra
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
		ContextWindow:  cfg.ContextWindow,
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
	if cfg.AllowedTools != nil {
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
		tools = kept
	}
	if len(cfg.DeniedTools) == 0 {
		return tools
	}
	denied := make(map[string]bool, len(cfg.DeniedTools))
	for _, n := range cfg.DeniedTools {
		denied[n] = true
	}
	kept := make([]bs.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if !denied[t.Name] {
			kept = append(kept, t)
		}
	}
	return kept
}

// ToolboxToolName is the loop-intercepted meta-tool that unlocks the full
// role toolset mid-turn. It is never registered in the ToolRegistry — the
// loop synthesizes its definition and services its calls itself.
const ToolboxToolName = "open_toolbox"

func toolboxToolDefinition() bs.ToolDefinition {
	return bs.ToolDefinition{
		Name:        ToolboxToolName,
		Description: "Unlock the full tool set for this turn. Call this when the user's request needs a tool that is not in your current tool list (for example web access, files, calendar, scheduling, memory, or integrations). Every tool available to your role is loaded immediately after this call — then make the real call. Do not tell the user you lack a tool without trying this first.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

// withToolboxTool appends the open_toolbox escape hatch when a narrowed
// override is active and an expansion set is configured.
func withToolboxTool(cfg RunConfig, tools []bs.ToolDefinition) []bs.ToolDefinition {
	if cfg.StrictTools || len(cfg.ToolboxExpansion) == 0 || cfg.ToolOverride == nil {
		return tools
	}
	out := make([]bs.ToolDefinition, 0, len(tools)+1)
	out = append(out, tools...)
	out = append(out, toolboxToolDefinition())
	return out
}

// toolboxTools resolves the expansion set through the same registry and
// AllowedTools filters as normal selection, so unlocking the toolbox can
// never expose a tool the soul's allowlist hides.
func (a *Loop) toolboxTools(cfg RunConfig) []bs.ToolDefinition {
	expanded := cfg
	expanded.ToolOverride = cfg.ToolboxExpansion
	return a.selectTools(expanded)
}

func toolDefinitionPresent(tools []bs.ToolDefinition, name string) bool {
	for _, def := range tools {
		if def.Name == name {
			return true
		}
	}
	return false
}

func visibleTextPointer(content []bs.ContentBlock) *string {
	text := bs.ExtractText(content)
	if text == "" {
		return nil
	}
	return &text
}

// feltTimeStaleAfter is the gap between the previous stored message and now
// beyond which the turn context carries an explicit elapsed-time reminder.
const feltTimeStaleAfter = 6 * time.Hour

// dialogGapMarkerAfter is the silence between two consecutive messages beyond
// which the window gets an explicit clock reading.
//
// The day marker alone left the model unable to tell two minutes from eight
// hours: inside one calendar day the window is an unbroken run of messages, so
// a morning exchange and an evening one read as one continuous conversation.
// That is how a reply came to place a training session and the report about it
// at the same moment, three hours apart — the model was not guessing wildly, it
// had nothing to guess from.
//
// 25 minutes marks the discontinuities a person would feel as "later" while
// staying quiet through the back-and-forth of an actual conversation. Too small
// and the window fills with markers nobody reads; too large and the afternoon
// collapses into the morning again.
const dialogGapMarkerAfter = 25 * time.Minute

const dialogDateFormat = "Monday 2006-01-02"
const dialogTimeFormat = "15:04"

// annotateDialogDays inserts user-role time markers into the rendered dialog
// window: [date: <Weekday YYYY-MM-DD> HH:MM] wherever the calendar day (in
// now's timezone) changes — including one before the first message, so the
// model can attribute every part of the transcript to a real day instead of
// reading the whole window as "today" — and [time: HH:MM] wherever the gap
// since the previous message exceeds dialogGapMarkerAfter, so it can also tell
// when a single day has a hole in it.
//
// Both markers are stable once emitted: each is derived from one message's own
// created_at, which never changes, so a marker inserted today reads identically
// tomorrow. That is what rules out the obvious alternative of stamping every
// message with a relative "N minutes ago" — the dialogue prefix has to stay
// byte-identical between turns or prompt caching cannot hold.
//
// No-op when now is zero. Markers are prompt-only; nothing is persisted.
func annotateDialogDays(msgs []bs.Message, now time.Time) []bs.Message {
	if now.IsZero() || len(msgs) == 0 {
		return msgs
	}
	loc := now.Location()
	out := make([]bs.Message, 0, len(msgs)+8)
	lastDay := ""
	var prev time.Time
	for _, m := range msgs {
		if !m.CreatedAt.IsZero() {
			at := m.CreatedAt.In(loc)
			day := at.Format(dialogDateFormat)
			switch {
			case day != lastDay:
				out = append(out, bs.Message{Role: "user",
					Content: "[date: " + day + " " + at.Format(dialogTimeFormat) + "]"})
				lastDay = day
			case !prev.IsZero() && at.Sub(prev) >= dialogGapMarkerAfter:
				out = append(out, bs.Message{Role: "user",
					Content: "[time: " + at.Format(dialogTimeFormat) + "]"})
			}
			prev = at
		}
		out = append(out, m)
	}
	return out
}

// feltTimeContext renders the [felt_time] turn-context block: what "now" is
// and how long ago the previous message landed. Empty when annotation is off,
// when there is no prior message, or when the previous message is recent and
// same-day (nothing to correct).
func feltTimeContext(msgs []bs.Message, now time.Time, currentInputPersisted bool) string {
	if now.IsZero() {
		return ""
	}
	// Interactive runs already persisted this turn's user input, so their
	// temporal anchor is one row earlier. Prompt-only runs persist no faux
	// user: their anchor is the latest stored dialogue message itself.
	start := len(msgs) - 1
	if currentInputPersisted {
		start--
	}
	var prev time.Time
	for i := start; i >= 0; i-- {
		if !msgs[i].CreatedAt.IsZero() {
			prev = msgs[i].CreatedAt
			break
		}
	}
	if prev.IsZero() {
		return ""
	}
	loc := now.Location()
	prevLocal := prev.In(loc)
	gap := now.Sub(prev)
	sameDay := prevLocal.Format(dialogDateFormat) == now.Format(dialogDateFormat)
	if sameDay && gap < feltTimeStaleAfter {
		return ""
	}
	gapText := fmt.Sprintf("%.0fh", gap.Hours())
	if gap < 2*time.Hour {
		gapText = fmt.Sprintf("%.0fm", gap.Minutes())
	}
	return fmt.Sprintf("[felt_time]\nNow: %s %s.\nPrevious message in this conversation: %s %s — %s ago. Everything above the last [date: ...] marker happened on earlier days; do not narrate it as today.\n[/felt_time]",
		now.Format(dialogDateFormat), now.Format("15:04"),
		prevLocal.Format(dialogDateFormat), prevLocal.Format("15:04"),
		gapText)
}

func toolboxUnlockedResult(tools []bs.ToolDefinition) string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return "Toolbox unlocked for this turn. Available tools now: " + strings.Join(names, ", ") + ". Make the call you need now."
}

// effectiveSystemPrompt returns the cacheable head of the prompt: the system
// prompt plus any compaction summary, and nothing that varies per turn.
//
// Per-turn material (memory traces, reflex guidance, felt time, tool
// observations) rides at the tail of the message array instead — see
// appendTurnContext. Providers cache by prefix, so anything dynamic placed here
// would invalidate not just the system block but the whole dialog behind it,
// which is otherwise byte-identical between calls.
func effectiveSystemPrompt(systemPrompt, compactSummary string) string {
	effective := systemPrompt
	if compactSummary != "" {
		effective += SummaryHeader + compactSummary
	}
	return effective
}

// dialogDatetimeFormat is how the current time is stamped for the model. Kept
// identical to the wording handlers used when they prepended it themselves, so
// the move changes where it is read, not what it says.
const dialogDatetimeFormat = "2006-01-02 15:04 MST (Monday)"

// withCurrentDatetime puts the clock at the head of the turn context.
//
// It used to lead the system prompt, which made it the single most expensive
// token in the request: it changes every minute and sat at position zero, so
// every new message invalidated the entire cached prefix behind it — system
// prompt, tools and the whole dialog. Measured on production before the move:
// 15.6% cache hit rate with 93 of 112 calls completely cold, while turns
// *within* one interaction — where the stamp is frozen — ran at 92-95%.
//
// The turn context is rebuilt every message anyway, so carrying the clock there
// costs nothing. It cannot simply be dropped: feltTimeContext stays empty
// during an active conversation, which leaves this stamp as the model's only
// clock most of the time.
//
// A zero time means the caller does not stamp turns (the handlers that build
// their own system prompt still prepend their own), and nothing is added.
func withCurrentDatetime(turnContext string, now time.Time) string {
	if now.IsZero() {
		return turnContext
	}
	stamp := "[current_datetime: " + now.Format(dialogDatetimeFormat) + "]"
	if strings.TrimSpace(turnContext) == "" {
		return stamp
	}
	return stamp + "\n\n" + turnContext
}

// turnContextEnvelope wraps the per-turn context in the same delimiters it
// carried when it lived in the system prompt, so relocating it changes where
// the model reads it, not what it reads.
func turnContextEnvelope(turnContext string) string {
	return "## Turn context\n[turn_context]\n" + turnContext + "\n[/turn_context]"
}

// turnContextAnchor is the index the per-turn context is pinned to: the current
// user turn, as it stands when the tool loop starts.
//
// Pinning matters. A tool loop grows the conversation by appending, so "the
// last message" names a different message on every turn — and moving the
// context along with it rewrites a message the previous turn already sent,
// breaking the prefix exactly where the loop needs it intact. Anchored, every
// later turn is a strict extension of the one before it and a prefix cache
// keeps matching right up to the newly appended tool traffic.
//
// This holds because the context is loop-invariant: reflex guidance, injected
// context, felt time and tool observations are all computed before the first
// turn. Only the available-tools shelf changes, and only on the forced-final
// turn, which gives up the cache from the anchor onward — once, at the end of a
// loop that has already paid for itself.
func turnContextAnchor(convo []bs.Message) int {
	return len(convo) - 1
}

// withTurnContext places the per-turn context at anchor, keeping tools, system
// and dialog history byte-stable ahead of it.
//
// It joins the anchored turn when that turn is the user's, and otherwise
// becomes its own user turn directly after it — which also leaves the wire
// array ending on a user message, as the Anthropic OAuth surface requires. The
// anchored turn is the user's own message, never one carrying tool results, so
// the context can never wedge between a tool call and its replies.
//
// messages is expected to be a clone (see cloneMessages); content slices are
// rebuilt rather than appended in place so a shared backing array is never
// written through.
func withTurnContext(messages []bs.Message, anchor int, turnContext string) []bs.Message {
	if strings.TrimSpace(turnContext) == "" {
		return messages
	}
	block := bs.ContentBlock{Type: "text", Text: turnContextEnvelope(turnContext)}

	if anchor >= 0 && anchor < len(messages) && messages[anchor].Role == "user" {
		blocks := bs.NormalizeContent(messages[anchor].Content)
		combined := make([]bs.ContentBlock, 0, len(blocks)+1)
		combined = append(combined, blocks...)
		combined = append(combined, block)
		messages[anchor].Content = combined
		return messages
	}

	ctxMsg := bs.Message{Role: "user", Content: []bs.ContentBlock{block}}
	if anchor < 0 || anchor+1 >= len(messages) {
		return append(messages, ctxMsg)
	}
	out := make([]bs.Message, 0, len(messages)+1)
	out = append(out, messages[:anchor+1]...)
	out = append(out, ctxMsg)
	out = append(out, messages[anchor+1:]...)
	return out
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
	// Non-empty case: the tool definitions already carry every name and
	// schema — re-listing the names here was pure duplication. One line
	// keeps the block's contract (scope = this turn only) without the list.
	b.WriteString("Only the native tool_use calls in your tool definitions are available in this turn.\n")
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
	// The turn context now travels with the messages rather than the system
	// prompt, but it still occupies prompt space, so it stays part of the
	// overhead the dialog budget has to make room for.
	promptOverhead := estimateTextTokens(effectiveSystemPrompt(systemPrompt, compactSummary)) + estimateToolSchemaTokens(tools)
	if strings.TrimSpace(turnContext) != "" {
		promptOverhead += estimateTextTokens(turnContextEnvelope(turnContext))
	}
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
		return 6
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
	directive := bs.ContentBlock{Type: "text", Text: "\n\n[tool_limit]\nYou hit this turn's tool-call budget, so tools are withheld while you write the final answer. This is a per-turn harness limit — your access and permissions are unchanged, and every tool returns on the next user message. Answer now from the results already gathered; do not stall asking to run more lookups. If the task is unfinished, say plainly that you hit the per-turn tool limit and offer to continue — never claim you lack access, rights, or write capability.\n[/tool_limit]"}
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

func (a *Loop) emptyVisibleFallback(cfg RunConfig) string {
	if fallback := strings.TrimSpace(cfg.EmptyVisibleFallback); fallback != "" {
		return fallback
	}
	if a != nil && a.cfg != nil && strings.TrimSpace(a.cfg.UI.ModelRefused) != "" {
		return a.cfg.UI.ModelRefused
	}
	return "(the model produced no visible answer — retry with a shorter request)"
}
