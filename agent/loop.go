package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/session"
)

// Loop orchestrates the send → tool_use → dispatch → loop cycle.
type Loop struct {
	provider  bs.CompletionProvider
	store     *session.Store
	registry  *bs.ToolRegistry
	compactor *Compactor // nil = disabled
	logger    *slog.Logger
	cfg       *bs.Config
}

// RunConfig controls agent loop execution.
type RunConfig struct {
	SessionID    string
	SystemPrompt string
	Model        string
	MaxTokens    int
	MaxTurns     int
}

// NewLoop creates a new agent loop.
func NewLoop(provider bs.CompletionProvider, store *session.Store, registry *bs.ToolRegistry, cfg *bs.Config, logger *slog.Logger) *Loop {
	return &Loop{
		provider: provider,
		store:    store,
		registry: registry,
		cfg:      cfg,
		logger:   logger,
	}
}

// SetCompactor enables conversation compaction.
func (a *Loop) SetCompactor(c *Compactor) {
	a.compactor = c
}

// Run executes the agent loop: sends messages to the LLM, dispatches tool calls, and loops
// until the LLM returns end_turn or max_tokens, or maxTurns is exceeded.
// Returns the final text response.
func (a *Loop) Run(ctx context.Context, cfg RunConfig, userMessage string) (string, error) {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = a.cfg.Gateway.MaxTurns
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = a.cfg.Limits.MaxOutputTokens
	}
	if cfg.Model == "" {
		cfg.Model = a.cfg.Models.Primary
	}

	// 1. Append user message
	_, err := a.store.Append(ctx, cfg.SessionID, bs.Message{Role: "user", Content: userMessage})
	if err != nil {
		return "", fmt.Errorf("append user message: %w", err)
	}

	tools := a.registry.Definitions()
	tokenBudget := a.calculateBudget(cfg.SystemPrompt, tools)

	// Pre-compute compaction once before the tool-use loop.
	var compactSummary string
	var compactedMessages []bs.Message

	if a.compactor != nil {
		preloadMsgs, loadErr := a.store.MessagesForAPI(ctx, cfg.SessionID, tokenBudget)
		if loadErr != nil {
			a.logger.Warn("compaction preload failed", "error", loadErr)
		} else if len(preloadMsgs) > 0 {
			summary, kept, compErr := a.compactor.Compact(ctx, preloadMsgs)
			if compErr != nil {
				a.logger.Warn("compaction failed", "error", compErr)
			} else if summary != "" {
				compactSummary = summary
				compactedMessages = kept
				a.logger.Info("compaction triggered",
					"original_msgs", len(preloadMsgs),
					"kept_msgs", len(kept),
				)
			}
		}
	}

	// Accumulate text across all turns.
	var accumulated strings.Builder

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		// 2. Build effective system prompt with compaction summary
		effectiveSystem := cfg.SystemPrompt
		if compactSummary != "" {
			effectiveSystem += "\n\n## Краткое содержание предыдущего разговора\n" + compactSummary
		}

		// 3. Load messages
		var messages []bs.Message
		if compactedMessages != nil {
			messages = compactedMessages
			compactedMessages = nil
		} else {
			messages, err = a.store.MessagesForAPI(ctx, cfg.SessionID, tokenBudget)
			if err != nil {
				return "", fmt.Errorf("load messages: %w", err)
			}
		}

		// 4. Call LLM
		resp, err := a.provider.Complete(ctx, bs.CompletionRequest{
			Model:     cfg.Model,
			MaxTokens: cfg.MaxTokens,
			System:    effectiveSystem,
			Messages:  messages,
			Tools:     tools,
		})
		if err != nil {
			return "", fmt.Errorf("LLM API: %w", err)
		}

		a.logger.Info("LLM response",
			"stop_reason", resp.StopReason,
			"input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens,
			"turn", turn+1,
		)

		// 5. Store assistant response
		_, err = a.store.AppendWithTokens(ctx, cfg.SessionID, bs.Message{
			Role:    "assistant",
			Content: resp.Content,
		}, resp.Usage.OutputTokens)
		if err != nil {
			return "", fmt.Errorf("append assistant message: %w", err)
		}

		// Collect text from this turn
		if turnText := session.ExtractText(resp.Content); turnText != "" {
			if accumulated.Len() > 0 {
				accumulated.WriteString("\n\n")
			}
			accumulated.WriteString(turnText)
		}

		// 6. Check stop reason
		switch resp.StopReason {
		case "end_turn", "max_tokens":
			return accumulated.String(), nil

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
					Content:   result,
					IsError:   isError,
				})
			}

			_, err = a.store.Append(ctx, cfg.SessionID, bs.Message{
				Role:    "user",
				Content: toolResults,
			})
			if err != nil {
				return "", fmt.Errorf("append tool results: %w", err)
			}

			continue

		default:
			return accumulated.String(), nil
		}
	}

	return "", fmt.Errorf("agent loop exceeded %d turns", cfg.MaxTurns)
}

// calculateBudget computes the token budget for message retrieval.
func (a *Loop) calculateBudget(systemPrompt string, tools []bs.ToolDefinition) int {
	maxContext := a.cfg.Limits.MaxContext

	systemTokens := len([]rune(systemPrompt)) / 3

	toolSchemaTokens := 0
	if len(tools) > 0 {
		data, _ := json.Marshal(tools)
		toolSchemaTokens = len(data) / 3
	}

	budget := maxContext - systemTokens - toolSchemaTokens
	if budget < 10000 {
		budget = 10000
	}
	return budget
}
