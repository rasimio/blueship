package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

// persistCtx returns a context for a critical session-state write (a message
// append) that survives the iteration's deadline/cancellation. A heavy agent
// iteration (many browser_fetch + a long synthesis turn) can run past its task
// budget mid-loop; the assistant message and tool results MUST still persist or
// the shared session is left with a dangling tool_use turn that breaks the next
// iteration ("append … : begin tx: context deadline exceeded"). Values (soul
// id, task id) are preserved; a fresh short deadline bounds the write.
func persistCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 20*time.Second)
}

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
		started := time.Now()
		err := a.store.Append(ctx, cfg.SessionID, bs.Message{
			Role:             "user",
			Content:          userMessage,
			ReplyToMessageID: cfg.ReplyToMessageID,
			TGMessageID:      cfg.TGMessageID,
		})
		emitTiming(cfg, "agent.append_user", started, "role="+cfg.Role)
		if err != nil {
			return nil, fmt.Errorf("append user message: %w", err)
		}
	}

	tools := a.selectTools(cfg)
	budgetDecision := a.effectiveMessageBudget(cfg, cfg.SystemPrompt, tools)
	tokenBudget := budgetDecision.Budget

	// Pre-existing compact summary from previous runs
	compactSummary := cfg.CompactSummary

	if a.compactor != nil {
		started := time.Now()
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
		emitTiming(cfg, "agent.compaction", started, "role="+cfg.Role)
	}

	turnContext := buildTurnContext(cfg.ReflexGuidance, cfg.InjectedContext)
	loadStarted := time.Now()
	dialogMessages, loadErr := a.store.DialogMessagesForAPI(ctx, cfg.SessionID, tokenBudget)
	emitTiming(cfg, "agent.load_dialog_messages", loadStarted, fmt.Sprintf("role=%s budget=%d", cfg.Role, tokenBudget))
	if loadErr != nil {
		return nil, fmt.Errorf("load dialog messages: %w", loadErr)
	}
	dialogTokens := estimateMessagesTokens(dialogMessages)
	convo := cloneMessages(dialogMessages)

	// Accumulate text and tool traces across all turns.
	var accumulated strings.Builder
	var traces []ToolTrace
	toolTurns := 0
	forceFinal := false

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		baseSystem := effectiveSystemPrompt(cfg.SystemPrompt, compactSummary, "")
		effectiveSystem := effectiveSystemPrompt(cfg.SystemPrompt, compactSummary, turnContext)
		messages := cloneMessages(convo)
		turnTools := tools
		if forceFinal {
			turnTools = nil
			if len(messages) > 0 {
				appendFinalAnswerDirective(&messages[len(messages)-1])
			}
		}
		scratchpadTokens := estimateMessagesTokens(convo) - dialogTokens
		if scratchpadTokens < 0 {
			scratchpadTokens = 0
		}

		// 4. Call LLM
		anatomy := newPromptAnatomy(a.registry, cfg.SystemPrompt, compactSummary, baseSystem, effectiveSystem, turnContext, turnTools, messages, dialogTokens, scratchpadTokens)
		callAttrs := []any{
			"model", cfg.Model,
			"role", cfg.Role,
			"tools", len(turnTools),
			"tool_selection_source", cfg.ToolSelectionSource,
			"messages", len(messages),
			"force_final", forceFinal,
			"message_budget", tokenBudget,
			"message_budget_source", budgetDecision.Source,
		}
		callAttrs = append(callAttrs, anatomy.logAttrs()...)
		a.logger.Info("calling LLM", callAttrs...)
		llmStarted := time.Now()
		resp, err := a.provider.Complete(ctx, bs.CompletionRequest{
			Model:          cfg.Model,
			MaxTokens:      cfg.MaxTokens,
			System:         effectiveSystem,
			Messages:       messages,
			Tools:          turnTools,
			ThinkingBudget: chooseThinkingBudget(cfg.ThinkingBudget, a.cfg.Limits.ThinkingBudget),
			ThinkingMode:   cfg.ThinkingMode,
			Effort:         cfg.Effort,
			Temperature:    cfg.Temperature,
		})
		if err != nil {
			return nil, fmt.Errorf("LLM API: %w", err)
		}
		emitTiming(cfg, "llm.complete", llmStarted, llmTimingDetail(cfg, turn+1, resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens))
		a.recordLLMUsage(ctx, cfg, cfg.Model, turnTools, messages, baseSystem, effectiveSystem, tokenBudget, budgetDecision.Source, turnContext, dialogTokens, scratchpadTokens, resp.Usage, resp.StopReason, llmStarted)

		responseAttrs := []any{
			"stop_reason", resp.StopReason,
			"input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens,
			"turn", turn + 1,
		}
		responseAttrs = append(responseAttrs, anatomy.responseAttrs(resp.Usage.InputTokens)...)
		a.logger.Info("LLM response", responseAttrs...)

		// 5. Store assistant response (skipped for an ephemeral run). Detached
		// ctx so a long turn that just consumed the iteration budget can't lose
		// this state write.
		assistantMsg := bs.Message{
			Role:    "assistant",
			Content: resp.Content,
		}
		if !cfg.Ephemeral {
			appendStarted := time.Now()
			pctx, pcancel := persistCtx(ctx)
			err = a.store.AppendWithTokens(pctx, cfg.SessionID, assistantMsg, resp.Usage.OutputTokens)
			pcancel()
			emitTiming(cfg, "agent.append_assistant", appendStarted, fmt.Sprintf("role=%s turn=%d", cfg.Role, turn+1))
			if err != nil {
				return nil, fmt.Errorf("append assistant message: %w", err)
			}
		}
		convo = append(convo, assistantMsg)

		// Collect this turn's text, de-duped (see appendTurnText — guards the
		// heartbeat "reminder prose + memory_update, then same reminder again"
		// double-message).
		appendTurnText(&accumulated, bs.ExtractText(resp.Content))

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
				text = a.cfg.UI.ModelRefused
			}
			return &RunResult{Text: text, ToolTraces: traces}, nil

		case "tool_use":
			toolTurns++
			var toolResults []bs.ContentBlock
			var promptToolResults []bs.ContentBlock
			for _, block := range resp.Content {
				if block.Type != "tool_use" {
					continue
				}

				a.logger.Info("executing tool",
					"tool", block.Name,
					"tool_use_id", block.ID,
				)

				toolStarted := time.Now()
				result, isError := a.registry.Execute(ctx, block.Name, block.Input)
				latencyMs := int(time.Since(toolStarted) / time.Millisecond)
				emitTiming(cfg, "tool.execute", toolStarted, toolTimingDetail(cfg, turn+1, block.Name, isError))
				a.logger.Info("tool result",
					"tool", block.Name,
					"tool_use_id", block.ID,
					"latency_ms", latencyMs,
					"is_error", isError,
					"input_bytes", len(block.Input),
					"output_bytes", len(result),
					"output_runes", len([]rune(result)),
				)
				resultBlock := bs.ContentBlock{
					Type:      "tool_result",
					ToolUseID: block.ID,
					Name:      block.Name,
					Content:   result,
					IsError:   isError,
				}
				toolResults = append(toolResults, resultBlock)
				promptToolResults = append(promptToolResults, compactToolResultBlockForPrompt(resultBlock))
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

			toolResultMsg := bs.Message{
				Role:    "user",
				Content: toolResults,
			}
			promptToolResultMsg := bs.Message{
				Role:    "user",
				Content: promptToolResults,
			}
			if !cfg.Ephemeral {
				appendStarted := time.Now()
				pctx, pcancel := persistCtx(ctx)
				err = a.store.Append(pctx, cfg.SessionID, toolResultMsg)
				pcancel()
				emitTiming(cfg, "agent.append_tool_results", appendStarted, fmt.Sprintf("role=%s turn=%d tools=%d", cfg.Role, turn+1, len(toolResults)))
				if err != nil {
					return nil, fmt.Errorf("append tool results: %w", err)
				}
			}
			convo = append(convo, promptToolResultMsg)
			if toolTurns >= maxToolTurnsForRole(cfg.Role) {
				forceFinal = true
				if turn+1 >= cfg.MaxTurns {
					cfg.MaxTurns = turn + 2
				}
				a.logger.Warn("tool turn budget exhausted; forcing final answer",
					"role", cfg.Role,
					"tool_turns", toolTurns,
					"next_turn", turn+2,
				)
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
