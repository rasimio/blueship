package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

func (a *Loop) RunStream(ctx context.Context, cfg RunConfig, userMessage any, cb *bs.StreamCallbacks) (string, []ToolTrace, error) {
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
	ctx = bs.WithDeniedTools(ctx, cfg.DeniedTools)

	if !cfg.SkipUserAppend && !cfg.PromptOnlyInput {
		started := time.Now()
		receipt, err := appendPersistedMessage(ctx, a.store, cfg.SessionID, bs.Message{
			Role:             "user",
			Content:          durableUserContent(cfg, userMessage),
			VisibleText:      cfg.VisibleUserText,
			ReplyToMessageID: cfg.ReplyToMessageID,
			TGMessageIDs:     cfg.TGMessageIDs,
		})
		emitTiming(cfg, "agent.append_user", started, "role="+cfg.Role)
		if err != nil {
			return "", nil, fmt.Errorf("append user message: %w", err)
		}
		if cfg.OnUserMessagePersisted != nil && receipt.ID != "" {
			cfg.OnUserMessagePersisted(ctx, receipt)
		}
	}

	tools := withToolboxTool(cfg, a.selectTools(cfg))
	budgetDecision := a.effectiveMessageBudget(cfg, cfg.SystemPrompt, tools)
	tokenBudget := budgetDecision.Budget
	compactSummary := cfg.CompactSummary
	toolObservationContext := a.recentToolObservationContext(ctx, cfg)
	turnContext := withCurrentDatetime(buildTurnContextForTools(cfg.ReflexGuidance, cfg.InjectedContext, tools, toolObservationContext), cfg.TurnNow)
	dialogDecision := effectiveDialogBudgetDecision(tokenBudget, cfg.SystemPrompt, compactSummary, turnContext, tools)
	dialogBudget := dialogBudgetAfterCurrentExpansion(
		dialogDecision.DialogBudget, cfg, userMessage,
	)
	promptOverhead := dialogDecision.PromptOverhead
	loadStarted := time.Now()
	dialogMessages, loadErr := a.store.DialogMessagesForAPI(ctx, cfg.SessionID, dialogBudget, compactSummary != "")
	emitTiming(cfg, "agent.load_dialog_messages", loadStarted, fmt.Sprintf("role=%s budget=%d effective_dialog_budget=%d prompt_overhead=%d mode=%s", cfg.Role, tokenBudget, dialogBudget, promptOverhead, dialogDecision.Mode))
	if loadErr != nil {
		return "", nil, fmt.Errorf("load dialog messages: %w", loadErr)
	}
	dialogMessages = overlayCurrentUserContent(dialogMessages, cfg, userMessage)
	feltTime := feltTimeContext(dialogMessages, cfg.TurnNow, !cfg.PromptOnlyInput)
	dialogMessages = annotateDialogDays(dialogMessages, cfg.TurnNow)
	dialogTokens := estimateMessagesTokens(dialogMessages)

	var accumulated strings.Builder
	var traces []ToolTrace
	toolTurns := 0
	forceFinal := false
	maxTokenContinuations := 0
	var pendingMaxTokenText strings.Builder
	pendingMaxTokenOutputTokens := 0
	// Prompt assembly keeps persisted visible dialogue and current-run tool
	// scratch separate. The DB remains the audit/UI store, but tool turns feed
	// the next LLM call through this in-memory transcript instead of consuming
	// the next turn's dialogue budget.
	convo := cloneMessages(dialogMessages)
	if cfg.PromptOnlyInput {
		convo = append(convo, bs.Message{Role: "user", Content: userMessage})
	}
	// Fixed for the whole loop: convo only ever grows at the tail, so this keeps
	// pointing at the current user turn while tool traffic accumulates behind it.
	ctxAnchor := turnContextAnchor(convo)

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		baseSystem := effectiveSystemPrompt(cfg.SystemPrompt, compactSummary)
		messages := cloneMessages(convo)
		turnTools := tools
		if forceFinal {
			turnTools = nil
		}
		turnContext := withCurrentDatetime(buildTurnContextForTools(cfg.ReflexGuidance, cfg.InjectedContext, turnTools, feltTime, toolObservationContext), cfg.TurnNow)
		messages = withTurnContext(messages, ctxAnchor, turnContext)
		if forceFinal && len(messages) > 0 {
			// After the turn context, so the directive stays the last thing read.
			appendFinalAnswerDirective(&messages[len(messages)-1])
		}
		effectiveSystem := baseSystem
		scratchpadTokens := estimateMessagesTokens(convo) - dialogTokens
		if scratchpadTokens < 0 {
			scratchpadTokens = 0
		}

		req := bs.CompletionRequest{
			Model:          cfg.Model,
			MaxTokens:      cfg.MaxTokens,
			ContextWindow:  cfg.ContextWindow,
			System:         effectiveSystem,
			Messages:       messages,
			Tools:          turnTools,
			ThinkingBudget: chooseThinkingBudget(cfg.ThinkingBudget, a.cfg.Limits.ThinkingBudget),
			ThinkingMode:   cfg.ThinkingMode,
			Effort:         cfg.Effort,
			Temperature:    cfg.Temperature,
		}

		anatomy := newPromptAnatomy(a.registry, cfg.SystemPrompt, compactSummary, baseSystem, effectiveSystem, turnContext, turnTools, messages, dialogTokens, scratchpadTokens)
		callAttrs := []any{
			"model", cfg.Model,
			"role", cfg.Role,
			"tools", len(turnTools),
			"tool_selection_source", cfg.ToolSelectionSource,
			"messages", len(messages),
			"turn", turn + 1,
			"force_final", forceFinal,
			"message_budget", tokenBudget,
			"message_budget_source", budgetDecision.Source,
			"dialog_effective_budget", dialogBudget,
			"prompt_overhead_tokens_estimate", promptOverhead,
			"dialog_budget_mode", dialogDecision.Mode,
			"prompt_overhead_exceeds_budget", dialogDecision.PromptOverheadExceedsBudget,
		}
		callAttrs = append(callAttrs, anatomy.logAttrs(cfg.SessionID)...)
		a.logger.Info("calling LLM (stream)", callAttrs...)

		llmStarted := time.Now()
		resp, err := streamProvider.StreamComplete(ctx, req, cb)
		if err != nil {
			return "", nil, fmt.Errorf("LLM API: %w", err)
		}
		if shouldRetryEmptyVisibleOutput(resp, req) {
			a.recordLLMUsage(ctx, cfg, cfg.Model, turnTools, messages, baseSystem, effectiveSystem, tokenBudget, budgetDecision.Source, turnContext, dialogTokens, scratchpadTokens, resp.Usage, resp.StopReason+":empty_visible", llmStarted)
			a.logger.Warn("LLM returned max_tokens with no visible output; retrying with reasoning disabled",
				"model", cfg.Model,
				"role", cfg.Role,
				"turn", turn+1,
				"input_tokens", resp.Usage.InputTokens,
				"output_tokens", resp.Usage.OutputTokens,
			)
			retryReq := withoutReasoning(req)
			llmStarted = time.Now()
			resp, err = streamProvider.StreamComplete(ctx, retryReq, cb)
			if err != nil {
				return "", nil, fmt.Errorf("LLM API retry without reasoning: %w", err)
			}
		}
		emitTiming(cfg, "llm.stream_complete", llmStarted, llmTimingDetail(cfg, turn+1, resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens))
		a.recordLLMUsage(ctx, cfg, cfg.Model, turnTools, messages, baseSystem, effectiveSystem, tokenBudget, budgetDecision.Source, turnContext, dialogTokens, scratchpadTokens, resp.Usage, resp.StopReason, llmStarted)

		// Per-turn usage report: web sinks render a live token-window
		// indicator that climbs as the session grows. Telegram / voice
		// sinks set OnUsage = nil and the call is a no-op.
		if cb != nil && cb.OnUsage != nil {
			cb.OnUsage(resp.Usage.InputTokens, resp.Usage.OutputTokens)
		}

		// Persist last input_tokens onto the session so /api/chat/history
		// can serve it to the web cabinet on page load (before the first
		// live `usage` SSE frame of the visit). Non-fatal on error —
		// just means the chip starts empty until the next turn.
		if resp.Usage.InputTokens > 0 {
			if perr := a.store.RecordLastInputTokens(ctx, cfg.SessionID, resp.Usage.InputTokens); perr != nil {
				a.logger.Warn("record last_input_tokens failed", "error", perr)
			}
		}

		responseAttrs := []any{
			"stop_reason", resp.StopReason,
			"input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens,
			"turn", turn + 1,
		}
		responseAttrs = append(responseAttrs, anatomy.responseAttrs(resp.Usage.InputTokens)...)
		a.logger.Info("LLM response", responseAttrs...)
		usedEmptyVisibleFallback := false
		if !hasVisibleOutput(resp.Content) && (resp.StopReason == "end_turn" || resp.StopReason == "max_tokens") {
			// A called-off turn arrives here looking exactly like a model that
			// produced nothing: the provider hands back an empty end_turn with
			// zero tokens. Falling back then writes "the model refused your
			// request — rephrase it" into the chat, which blames the person for
			// pressing a button we shipped. Observed live 2026-08-12, on
			// somebody's first minute with the product. Cancellation has its own
			// path upstream; leave the reply to it and say nothing here.
			if ctx.Err() != nil {
				return accumulated.String(), traces, ctx.Err()
			}
			text := a.emptyVisibleFallback(cfg)
			a.logger.Warn("LLM returned terminal response with no visible output; using fallback",
				"model", cfg.Model,
				"role", cfg.Role,
				"turn", turn+1,
				"stop_reason", resp.StopReason,
			)
			resp.Content = []bs.ContentBlock{{Type: "text", Text: text}}
			usedEmptyVisibleFallback = true
			if cb != nil && cb.OnText != nil {
				cb.OnText(text)
			}
		}
		currentTurnText := bs.ExtractText(resp.Content)
		if !usedEmptyVisibleFallback && shouldAutoContinueMaxTokens(resp, maxTokenContinuations) {
			appendTurnText(&pendingMaxTokenText, currentTurnText)
			appendTurnText(&accumulated, currentTurnText)
			pendingMaxTokenOutputTokens += resp.Usage.OutputTokens
			convo = append(convo,
				bs.Message{Role: "assistant", Content: resp.Content},
				maxTokenContinuationMessage(),
			)
			maxTokenContinuations++
			forceFinal = true
			if turn+1 >= cfg.MaxTurns {
				cfg.MaxTurns = turn + 2
			}
			a.logger.Warn("LLM returned max_tokens with visible output; continuing answer",
				"model", cfg.Model,
				"role", cfg.Role,
				"turn", turn+1,
				"next_turn", turn+2,
			)
			continue
		}
		appendTokens := resp.Usage.OutputTokens
		if pendingText := pendingMaxTokenText.String(); pendingText != "" {
			resp.Content = []bs.ContentBlock{{Type: "text", Text: mergeContinuationText(pendingText, currentTurnText)}}
			appendTokens += pendingMaxTokenOutputTokens
		}

		assistantMsg := bs.Message{
			Role:        "assistant",
			Content:     resp.Content,
			VisibleText: visibleTextPointer(resp.Content),
		}
		if !cfg.Ephemeral {
			appendStarted := time.Now()
			pctx, pcancel := persistCtx(ctx)
			err = a.store.AppendWithTokens(pctx, cfg.SessionID, assistantMsg, appendTokens)
			pcancel()
			emitTiming(cfg, "agent.append_assistant", appendStarted, fmt.Sprintf("role=%s turn=%d", cfg.Role, turn+1))
			if err != nil {
				return "", nil, fmt.Errorf("append assistant message: %w", err)
			}
		}
		convo = append(convo, assistantMsg)

		// Collect this turn's text, de-duped (see appendTurnText).
		appendTurnText(&accumulated, currentTurnText)
		pendingMaxTokenText.Reset()
		pendingMaxTokenOutputTokens = 0

		switch resp.StopReason {
		case "end_turn", "max_tokens":
			return accumulated.String(), traces, nil

		case "refusal":
			// Same as Run() — surface explicitly so the user gets feedback.
			a.logger.Warn("LLM refused to respond (stream)", "model", cfg.Model, "turn", turn+1)
			text := accumulated.String()
			if cfg.PromptOnlyInput && cfg.Ephemeral &&
				strings.TrimSpace(cfg.EmptyVisibleFallback) != "" {
				// A private autonomous draft must never turn provider safety
				// copy into a user-facing attempt at contact.
				text = a.emptyVisibleFallback(cfg)
			} else if text == "" {
				text = a.cfg.UI.ModelRefused
				if cb != nil && cb.OnText != nil {
					cb.OnText(text)
				}
			}
			return text, traces, nil

		case "tool_use":
			toolTurns++
			var toolResults []bs.ContentBlock
			var promptToolResults []bs.ContentBlock
			for _, block := range resp.Content {
				if block.Type != "tool_use" {
					continue
				}
				if cfg.StrictTools && !toolDefinitionPresent(turnTools, block.Name) {
					result := fmt.Sprintf("tool %q is not allowed by the strict turn policy", block.Name)
					input := string(block.Input)
					if len(input) > 200 {
						input = input[:200] + "..."
					}
					a.logger.Warn("strict tool policy denied dispatch (stream)",
						"role", cfg.Role,
						"tool", block.Name,
						"tool_use_id", block.ID,
					)
					if cb != nil && cb.OnToolResult != nil {
						cb.OnToolResult(block.ID, result, true, 0)
					}
					traces = append(traces, ToolTrace{Name: block.Name, Input: input, Output: result, Error: true})
					resultBlock := bs.ContentBlock{
						Type:      "tool_result",
						ToolUseID: block.ID,
						Name:      block.Name,
						Content:   result,
						IsError:   true,
					}
					toolResults = append(toolResults, resultBlock)
					promptToolResults = append(promptToolResults, compactToolResultBlockForPrompt(resultBlock))
					continue
				}
				if block.Name == ToolboxToolName && len(cfg.ToolboxExpansion) > 0 {
					tools = a.toolboxTools(cfg)
					result := toolboxUnlockedResult(tools)
					a.logger.Info("toolbox unlocked (stream)", "role", cfg.Role, "tools", len(tools), "turn", turn+1)
					if cb != nil && cb.OnToolResult != nil {
						cb.OnToolResult(block.ID, result, false, 0)
					}
					traces = append(traces, ToolTrace{Name: block.Name, Input: "{}", Output: result})
					resultBlock := bs.ContentBlock{
						Type:      "tool_result",
						ToolUseID: block.ID,
						Name:      block.Name,
						Content:   result,
					}
					toolResults = append(toolResults, resultBlock)
					promptToolResults = append(promptToolResults, compactToolResultBlockForPrompt(resultBlock))
					continue
				}
				a.logger.Info("executing tool", "tool", block.Name, "tool_use_id", block.ID)
				start := time.Now()
				toolTimeout := resolveToolExecutionTimeout(cfg.ToolTimeout, block.Name)
				result, isError, timedOut := executeToolWithTimeout(ctx, a.registry, block.Name, block.Input, toolTimeout)
				latencyMs := int(time.Since(start) / time.Millisecond)
				emitTiming(cfg, "tool.execute", start, toolTimingDetail(cfg, turn+1, block.Name, isError))
				a.logger.Info("tool result",
					"tool", block.Name,
					"tool_use_id", block.ID,
					"latency_ms", latencyMs,
					"is_error", isError,
					"timed_out", timedOut,
					"input_bytes", len(block.Input),
					"output_bytes", len(result),
					"output_runes", len([]rune(result)),
				)
				if cb != nil && cb.OnToolResult != nil {
					cb.OnToolResult(block.ID, result, isError, latencyMs)
				}
				inputStr := string(block.Input)
				if len(inputStr) > 200 {
					inputStr = inputStr[:200] + "..."
				}
				outputStr := result
				if len(outputStr) > 500 {
					outputStr = outputStr[:500] + "..."
				}
				traces = append(traces, ToolTrace{Name: block.Name, Input: inputStr, Output: outputStr, Error: isError})
				resultBlock := bs.ContentBlock{
					Type:      "tool_result",
					ToolUseID: block.ID,
					Name:      block.Name,
					Content:   result,
					IsError:   isError,
				}
				toolResults = append(toolResults, resultBlock)
				promptToolResults = append(promptToolResults, compactToolResultBlockForPrompt(resultBlock))
			}

			// See RunTracked: a tool_use stop with no tool_use blocks must not
			// append an empty tool_results turn (it would be dropped, leaving
			// the wire array ending on an assistant turn — rejected as prefill
			// by the OAuth surface). Treat as terminal.
			if len(toolResults) == 0 {
				a.logger.Warn("tool_use stop with no tool_use blocks; treating as terminal (stream)", "turn", turn+1)
				return accumulated.String(), traces, nil
			}

			toolResultMsg := bs.Message{Role: "user", Content: toolResults}
			promptToolResultMsg := bs.Message{Role: "user", Content: promptToolResults}
			if !cfg.Ephemeral {
				appendStarted := time.Now()
				pctx, pcancel := persistCtx(ctx)
				err = a.store.Append(pctx, cfg.SessionID, toolResultMsg)
				pcancel()
				emitTiming(cfg, "agent.append_tool_results", appendStarted, fmt.Sprintf("role=%s turn=%d tools=%d", cfg.Role, turn+1, len(toolResults)))
				if err != nil {
					return "", nil, fmt.Errorf("append tool results: %w", err)
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

// chooseThinkingBudget picks the effective budget:
//   - cfg < 0  → explicit disable (0 to provider; both Anthropic and Ollama
//     treat 0 as "no extended thinking")
//   - cfg > 0  → explicit per-RunConfig budget
//   - cfg == 0 → inherit a.cfg.Limits.ThinkingBudget (the global default)
//
// Older code passed normalizeThinkingBudget(globalLimit) which returned -1
// when the global was 0 — Ollama then treated -1 != 0 as "thinking on" and
// the reflex tier paid 5-6 s of hidden tokens per turn. Don't bring that
// back: 0 reaches providers as 0.
