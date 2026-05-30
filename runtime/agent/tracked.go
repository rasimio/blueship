package agent

import (
	"context"
	"fmt"
	"strings"

	bs "github.com/rasimio/blueship/internal/core"
)

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
		if err := a.store.Append(ctx, cfg.SessionID, bs.Message{
			Role:             "user",
			Content:          userMessage,
			ReplyToMessageID: cfg.ReplyToMessageID,
			TGMessageID:      cfg.TGMessageID,
		}); err != nil {
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
	if cfg.MessageBudget > 0 {
		tokenBudget = cfg.MessageBudget
	}

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
			ThinkingBudget: chooseThinkingBudget(cfg.ThinkingBudget, a.cfg.Limits.ThinkingBudget),
			ThinkingMode:   cfg.ThinkingMode,
			Effort:         cfg.Effort,
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

		case "refusal":
			// Anthropic safety classifier refused (introduced 2025-late).
			// Falling through to default returned empty text and the gateway
			// silently sent nothing — the user just saw the chat stop. Surface
			// it explicitly so the user gets feedback and can rephrase.
			a.logger.Warn("LLM refused to respond", "model", cfg.Model, "turn", turn+1)
			text := accumulated.String()
			if text == "" {
				text = "(модель отказалась отвечать на этот запрос — переформулируй / упрости контекст)"
			}
			return &RunResult{Text: text, ToolTraces: traces}, nil

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

			// Defensive: stop_reason was "tool_use" but no tool_use blocks
			// materialised (e.g. content was thinking/text-only after the
			// provider's thinking-block filter). Appending an empty
			// tool_results user message would be dropped by the provider's
			// content normaliser, leaving the wire array ending on an
			// assistant turn — which the Anthropic OAuth surface rejects as
			// prefill. Treat the turn as terminal rather than looping on an
			// empty round-trip.
			if len(toolResults) == 0 {
				a.logger.Warn("tool_use stop with no tool_use blocks; treating as terminal", "turn", turn+1)
				return &RunResult{Text: accumulated.String(), ToolTraces: traces}, nil
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

// RunStream is like Run but streams events via cb. cb.OnText fires for each
// text delta from the LLM; cb.OnToolUse fires when the LLM emits a tool call;
// cb.OnToolResult fires after the agent loop executes the tool; cb.OnThinking
// fires for thinking deltas (Anthropic). cb may be nil to suppress all events
// (degrades to batch-like behavior).
//
// Used by voice transport for sentence-level TTS pipelining, by Telegram for
// progressive message editing, and by the web cabinet for full tool-use
// inspector rendering. Returns the reply text, tool traces (for debug/audit),
// and any error.
