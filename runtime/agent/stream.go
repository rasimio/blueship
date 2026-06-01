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

	if !cfg.SkipUserAppend {
		if err := a.store.Append(ctx, cfg.SessionID, bs.Message{
			Role:             "user",
			Content:          userMessage,
			ReplyToMessageID: cfg.ReplyToMessageID,
			TGMessageID:      cfg.TGMessageID,
		}); err != nil {
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
	if cfg.MessageBudget > 0 {
		tokenBudget = cfg.MessageBudget
	}
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
			ThinkingBudget: chooseThinkingBudget(cfg.ThinkingBudget, a.cfg.Limits.ThinkingBudget),
			ThinkingMode:   cfg.ThinkingMode,
			Effort:         cfg.Effort,
			Temperature:    cfg.Temperature,
		}

		a.logger.Info("calling LLM (stream)", "model", cfg.Model, "tools", len(tools), "messages", len(messages), "turn", turn+1)

		resp, err := streamProvider.StreamComplete(ctx, req, cb)
		if err != nil {
			return "", nil, fmt.Errorf("LLM API: %w", err)
		}

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

		case "refusal":
			// Same as Run() — surface explicitly so the user gets feedback.
			a.logger.Warn("LLM refused to respond (stream)", "model", cfg.Model, "turn", turn+1)
			text := accumulated.String()
			if text == "" {
				text = a.cfg.UI.ModelRefused
				if cb != nil && cb.OnText != nil {
					cb.OnText(text)
				}
			}
			return text, traces, nil

		case "tool_use":
			var toolResults []bs.ContentBlock
			for _, block := range resp.Content {
				if block.Type != "tool_use" {
					continue
				}
				a.logger.Info("executing tool", "tool", block.Name, "tool_use_id", block.ID)
				start := time.Now()
				result, isError := a.registry.Execute(ctx, block.Name, block.Input)
				latencyMs := int(time.Since(start) / time.Millisecond)
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
				toolResults = append(toolResults, bs.ContentBlock{
					Type:      "tool_result",
					ToolUseID: block.ID,
					Name:      block.Name,
					Content:   result,
					IsError:   isError,
				})
			}

			// See RunTracked: a tool_use stop with no tool_use blocks must not
			// append an empty tool_results turn (it would be dropped, leaving
			// the wire array ending on an assistant turn — rejected as prefill
			// by the OAuth surface). Treat as terminal.
			if len(toolResults) == 0 {
				a.logger.Warn("tool_use stop with no tool_use blocks; treating as terminal (stream)", "turn", turn+1)
				return accumulated.String(), traces, nil
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
