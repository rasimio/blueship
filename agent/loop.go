package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	bs "github.com/rasimio/blueship/core"
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
func (a *Loop) RunTracked(ctx context.Context, cfg RunConfig, userMessage any) (*RunResult, error) {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = a.cfg.Gateway.MaxTurns
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = a.cfg.Limits.MaxOutputTokens
	}
	if cfg.Model == "" {
		cfg.Model = a.cfg.Models.Primary.Name
	}

	// 1. Append user message (unless the caller already persisted it).
	if !cfg.SkipUserAppend {
		if err := a.store.Append(ctx, cfg.SessionID, bs.Message{Role: "user", Content: userMessage}); err != nil {
			return nil, fmt.Errorf("append user message: %w", err)
		}
	}

	// Select tools: ToolOverride > Role-based > all.
	var tools []bs.ToolDefinition
	if cfg.ToolOverride != nil {
		// Reflex explicitly selected tools (empty slice = no tools).
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
	// Apply the per-soul allowlist (Vaelum cabinet tool config), if set.
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
	tokenBudget := a.calculateBudget(cfg.SystemPrompt, tools)

	// Pre-existing compact summary from previous runs
	compactSummary := cfg.CompactSummary

	if a.compactor != nil {
		preloadMsgs, loadErr := a.store.AllMessagesForAPI(ctx, cfg.SessionID)
		if loadErr != nil {
			a.logger.Warn("compaction preload failed", "error", loadErr)
		} else if len(preloadMsgs) > 0 {
			summary, kept, compErr := a.compactor.Compact(ctx, preloadMsgs)
			if compErr != nil {
				a.logger.Warn("compaction failed", "error", compErr)
			} else if summary != "" {
				// Persist: delete old messages, save summary
				if err := a.store.CompactSession(ctx, cfg.SessionID, summary, len(kept)); err != nil {
					a.logger.Warn("compaction persist failed", "error", err)
				} else {
					if compactSummary != "" {
						compactSummary += "\n\n---\n\n" + summary
					} else {
						compactSummary = summary
					}
					a.logger.Info("compaction persisted",
						"original_msgs", len(preloadMsgs),
						"kept_msgs", len(kept),
					)
				}
			}
		}
	}

	// Accumulate text and tool traces across all turns.
	var accumulated strings.Builder
	var traces []ToolTrace

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		// 2. Build effective system prompt with compaction summary
		effectiveSystem := cfg.SystemPrompt
		if compactSummary != "" {
			effectiveSystem += SummaryHeader + compactSummary
		}

		// 3. Load messages (always from DB — compaction already persisted)
		messages, err := a.store.MessagesForAPI(ctx, cfg.SessionID, tokenBudget)
		if err != nil {
			return nil, fmt.Errorf("load messages: %w", err)
		}

		// On the first turn, prepend reflex guidance + injected context to the last user message.
		// Not stored in DB — ephemeral context (e.g. memory traces, matched rules).
		if turn == 0 && cfg.ReflexGuidance != "" && cfg.InjectedContext != "" {
			cfg.InjectedContext = cfg.ReflexGuidance + "\n\n" + cfg.InjectedContext
		} else if turn == 0 && cfg.ReflexGuidance != "" {
			cfg.InjectedContext = cfg.ReflexGuidance
		}
		if turn == 0 && cfg.InjectedContext != "" && len(messages) > 0 {
			last := &messages[len(messages)-1]
			if last.Role == "user" {
				blocks := bs.NormalizeContent(last.Content)
				prefix := bs.ContentBlock{Type: "text", Text: "[context]\n" + cfg.InjectedContext + "[/context]\n\n"}
				last.Content = append([]bs.ContentBlock{prefix}, blocks...)
			}
		}

		// 4. Call LLM
		a.logger.Info("calling LLM", "model", cfg.Model, "tools", len(tools), "messages", len(messages))
		resp, err := a.provider.Complete(ctx, bs.CompletionRequest{
			Model:          cfg.Model,
			MaxTokens:      cfg.MaxTokens,
			System:         effectiveSystem,
			Messages:       messages,
			Tools:          tools,
			ThinkingBudget: normalizeThinkingBudget(a.cfg.Limits.ThinkingBudget),
			Temperature:    cfg.Temperature,
		})
		if err != nil {
			return nil, fmt.Errorf("LLM API: %w", err)
		}

		a.logger.Info("LLM response",
			"stop_reason", resp.StopReason,
			"input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens,
			"turn", turn+1,
		)

		// 5. Store assistant response (skipped for an ephemeral run).
		if !cfg.Ephemeral {
			err = a.store.AppendWithTokens(ctx, cfg.SessionID, bs.Message{
				Role:    "assistant",
				Content: resp.Content,
			}, resp.Usage.OutputTokens)
			if err != nil {
				return nil, fmt.Errorf("append assistant message: %w", err)
			}
		}

		// Collect text from this turn
		if turnText := bs.ExtractText(resp.Content); turnText != "" {
			if accumulated.Len() > 0 {
				accumulated.WriteString("\n\n")
			}
			accumulated.WriteString(turnText)
		}

		// 6. Check stop reason
		switch resp.StopReason {
		case "end_turn", "max_tokens":
			return &RunResult{Text: accumulated.String(), ToolTraces: traces}, nil

		case "tool_use":
			var toolResults []bs.ContentBlock
			for _, block := range resp.Content {
				if block.Type != "tool_use" {
					continue
				}

				a.logger.Info("executing tool",
					"tool", block.Name,
					"tool_use_id", block.ID,
				)

				result, isError := a.registry.Execute(ctx, block.Name, block.Input)
				toolResults = append(toolResults, bs.ContentBlock{
					Type:      "tool_result",
					ToolUseID: block.ID,
					Name:      block.Name,
					Content:   result,
					IsError:   isError,
				})
				inputStr := string(block.Input)
				if len(inputStr) > 200 {
					inputStr = inputStr[:200] + "..."
				}
				outputStr := result
				if len(outputStr) > 500 {
					outputStr = outputStr[:500] + "..."
				}
				traces = append(traces, ToolTrace{Name: block.Name, Input: inputStr, Output: outputStr, Error: isError})
			}

			if !cfg.Ephemeral {
				err = a.store.Append(ctx, cfg.SessionID, bs.Message{
					Role:    "user",
					Content: toolResults,
				})
				if err != nil {
					return nil, fmt.Errorf("append tool results: %w", err)
				}
			}

			continue

		default:
			return &RunResult{Text: accumulated.String(), ToolTraces: traces}, nil
		}
	}

	// Return whatever text/traces accumulated before hitting the turn limit.
	// A turn that produced text or called a tool (e.g. an escalation pass run
	// with MaxTurns:1) is a valid result, not a failure.
	if text := accumulated.String(); text != "" || len(traces) > 0 {
		if text != "" {
			a.logger.Warn("agent loop hit turn limit, returning partial response", "turns", cfg.MaxTurns)
		}
		return &RunResult{Text: text, ToolTraces: traces}, nil
	}
	return nil, fmt.Errorf("agent loop exceeded %d turns with no text output", cfg.MaxTurns)
}

// RunStream is like Run but streams text chunks via onText callback.
// Used by voice transport for sentence-level TTS pipelining, and by Telegram
// for progressive message editing. onText fires for each text chunk from the
// LLM. onToolUse fires before each tool is executed (nil = ignore).
// Tool call turns use batch mode; only the final text response is streamed.
// Returns the reply text, tool traces (for debug/audit), and any error.
func (a *Loop) RunStream(ctx context.Context, cfg RunConfig, userMessage any, onText func(string), onToolUse func(name string)) (string, []ToolTrace, error) {
	streamProvider, ok := a.provider.(bs.StreamCompletionProvider)
	if !ok {
		// Fallback to batch if provider doesn't support streaming
		text, err := a.Run(ctx, cfg, userMessage)
		return text, nil, err
	}

	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = a.cfg.Gateway.MaxTurns
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = a.cfg.Limits.MaxOutputTokens
	}
	if cfg.Model == "" {
		cfg.Model = a.cfg.Models.Primary.Name
	}

	if !cfg.SkipUserAppend {
		if err := a.store.Append(ctx, cfg.SessionID, bs.Message{Role: "user", Content: userMessage}); err != nil {
			return "", nil, fmt.Errorf("append user message: %w", err)
		}
	}

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
	// Apply the per-soul allowlist (Vaelum cabinet tool config), if set.
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
	tokenBudget := a.calculateBudget(cfg.SystemPrompt, tools)
	compactSummary := cfg.CompactSummary

	var accumulated strings.Builder
	var traces []ToolTrace

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		effectiveSystem := cfg.SystemPrompt
		if compactSummary != "" {
			effectiveSystem += SummaryHeader + compactSummary
		}

		messages, err := a.store.MessagesForAPI(ctx, cfg.SessionID, tokenBudget)
		if err != nil {
			return "", nil, fmt.Errorf("load messages: %w", err)
		}

		if turn == 0 && cfg.ReflexGuidance != "" && cfg.InjectedContext != "" {
			cfg.InjectedContext = cfg.ReflexGuidance + "\n\n" + cfg.InjectedContext
		} else if turn == 0 && cfg.ReflexGuidance != "" {
			cfg.InjectedContext = cfg.ReflexGuidance
		}
		if turn == 0 && cfg.InjectedContext != "" && len(messages) > 0 {
			last := &messages[len(messages)-1]
			if last.Role == "user" {
				blocks := bs.NormalizeContent(last.Content)
				prefix := bs.ContentBlock{Type: "text", Text: "[context]\n" + cfg.InjectedContext + "[/context]\n\n"}
				last.Content = append([]bs.ContentBlock{prefix}, blocks...)
			}
		}

		req := bs.CompletionRequest{
			Model:          cfg.Model,
			MaxTokens:      cfg.MaxTokens,
			System:         effectiveSystem,
			Messages:       messages,
			Tools:          tools,
			ThinkingBudget: normalizeThinkingBudget(a.cfg.Limits.ThinkingBudget),
			Temperature:    cfg.Temperature,
		}

		a.logger.Info("calling LLM (stream)", "model", cfg.Model, "tools", len(tools), "messages", len(messages), "turn", turn+1)

		// Stream the LLM call — onText fires for each text chunk
		resp, err := streamProvider.StreamComplete(ctx, req, onText)
		if err != nil {
			return "", nil, fmt.Errorf("LLM API: %w", err)
		}

		a.logger.Info("LLM response",
			"stop_reason", resp.StopReason,
			"input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens,
			"turn", turn+1,
		)

		if !cfg.Ephemeral {
			err = a.store.AppendWithTokens(ctx, cfg.SessionID, bs.Message{
				Role:    "assistant",
				Content: resp.Content,
			}, resp.Usage.OutputTokens)
			if err != nil {
				return "", nil, fmt.Errorf("append assistant message: %w", err)
			}
		}

		if turnText := bs.ExtractText(resp.Content); turnText != "" {
			if accumulated.Len() > 0 {
				accumulated.WriteString("\n\n")
			}
			accumulated.WriteString(turnText)
		}

		switch resp.StopReason {
		case "end_turn", "max_tokens":
			return accumulated.String(), traces, nil

		case "tool_use":
			var toolResults []bs.ContentBlock
			for _, block := range resp.Content {
				if block.Type != "tool_use" {
					continue
				}
				a.logger.Info("executing tool", "tool", block.Name, "tool_use_id", block.ID)
				if onToolUse != nil {
					onToolUse(block.Name)
				}
				result, isError := a.registry.Execute(ctx, block.Name, block.Input)
				inputStr := string(block.Input)
				if len(inputStr) > 200 {
					inputStr = inputStr[:200] + "..."
				}
				outputStr := result
				if len(outputStr) > 500 {
					outputStr = outputStr[:500] + "..."
				}
				traces = append(traces, ToolTrace{Name: block.Name, Input: inputStr, Output: outputStr, Error: isError})
				toolResults = append(toolResults, bs.ContentBlock{
					Type:      "tool_result",
					ToolUseID: block.ID,
					Name:      block.Name,
					Content:   result,
					IsError:   isError,
				})
			}

			if !cfg.Ephemeral {
				err = a.store.Append(ctx, cfg.SessionID, bs.Message{Role: "user", Content: toolResults})
				if err != nil {
					return "", nil, fmt.Errorf("append tool results: %w", err)
				}
			}
			continue

		default:
			return accumulated.String(), traces, nil
		}
	}

	// A turn that produced text or called a tool (e.g. an escalation pass run
	// with MaxTurns:1) is a valid result, not a failure.
	if text := accumulated.String(); text != "" || len(traces) > 0 {
		return text, traces, nil
	}
	return "", traces, fmt.Errorf("agent loop exceeded %d turns with no text output", cfg.MaxTurns)
}

// calculateBudget computes the token budget for message retrieval.
func normalizeThinkingBudget(budget int) int {
	if budget == 0 {
		return -1
	}
	return budget
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
