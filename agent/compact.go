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

// SummaryHeader is prepended to the compaction summary when injected into the system prompt.
const SummaryHeader = "\n\n## Краткое содержание предыдущего разговора\n"

// Compactor summarizes older messages via a lightweight model to reduce context size.
type Compactor struct {
	provider     bs.CompletionProvider
	logger       *slog.Logger
	model        string
	maxTokens    int
	threshold    int
	keepTokens   int
	systemPrompt string
}

// NewCompactor creates a new Compactor. Returns nil if provider is nil.
func NewCompactor(provider bs.CompletionProvider, cfg *bs.Config, logger *slog.Logger) *Compactor {
	if provider == nil {
		return nil
	}
	return &Compactor{
		provider:   provider,
		logger:     logger,
		model:      cfg.Models.Compact,
		maxTokens:  cfg.Limits.CompactOutput,
		threshold:  cfg.Limits.CompactThreshold,
		keepTokens: cfg.Limits.CompactKeep,
	}
}

// SetSystemPrompt sets the compaction system prompt (loaded from .md file).
func (c *Compactor) SetSystemPrompt(prompt string) {
	c.systemPrompt = prompt
}

// Compact checks if messages exceed the token threshold and, if so, summarizes
// older messages. Returns the summary string and the kept (recent) messages.
func (c *Compactor) Compact(ctx context.Context, messages []bs.Message) (string, []bs.Message, error) {
	totalTokens := estimateMessagesTokens(messages)

	if totalTokens < c.threshold {
		return "", messages, nil
	}

	splitIdx := findSplitPoint(messages, c.keepTokens)
	if splitIdx <= 0 {
		return "", messages, nil
	}

	compactZone := messages[:splitIdx]
	keepZone := messages[splitIdx:]

	dialogue := formatForCompaction(compactZone)
	if dialogue == "" {
		return "", messages, nil
	}

	c.logger.Info("compacting conversation",
		"total_tokens", totalTokens,
		"compact_msgs", len(compactZone),
		"keep_msgs", len(keepZone),
	)

	summary, err := c.summarize(ctx, dialogue)
	if err != nil {
		return "", nil, fmt.Errorf("summarize: %w", err)
	}

	return summary, keepZone, nil
}

func (c *Compactor) summarize(ctx context.Context, dialogue string) (string, error) {
	resp, err := c.provider.Complete(ctx, bs.CompletionRequest{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System:    c.systemPrompt,
		Messages:  []bs.Message{{Role: "user", Content: dialogue}},
	})
	if err != nil {
		return "", err
	}
	return session.ExtractText(resp.Content), nil
}

func estimateMessagesTokens(messages []bs.Message) int {
	total := 0
	for _, msg := range messages {
		blocks := bs.NormalizeContent(msg.Content)
		total += bs.EstimateTokens(blocks)
	}
	return total
}

func findSplitPoint(messages []bs.Message, keep int) int {
	tokenSum := 0
	splitIdx := len(messages)

	for i := len(messages) - 1; i >= 0; i-- {
		blocks := bs.NormalizeContent(messages[i].Content)
		tokenSum += bs.EstimateTokens(blocks)
		if tokenSum >= keep {
			splitIdx = i
			break
		}
	}

	if splitIdx > 0 && splitIdx < len(messages) {
		if hasToolResult(messages[splitIdx]) && splitIdx > 0 {
			splitIdx--
		}
		if hasToolUse(messages[splitIdx]) && splitIdx+1 < len(messages) {
			splitIdx++
			if splitIdx < len(messages) && hasToolResult(messages[splitIdx]) {
				splitIdx++
			}
		}
	}

	for splitIdx < len(messages) && messages[splitIdx].Role != "user" {
		splitIdx++
	}

	if splitIdx >= len(messages) {
		return 0
	}

	return splitIdx
}

func hasToolUse(msg bs.Message) bool {
	blocks := bs.NormalizeContent(msg.Content)
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}

func hasToolResult(msg bs.Message) bool {
	blocks := bs.NormalizeContent(msg.Content)
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

func formatForCompaction(messages []bs.Message) string {
	var sb strings.Builder

	for _, msg := range messages {
		blocks := bs.NormalizeContent(msg.Content)
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text == "" {
					continue
				}
				role := "user"
				if msg.Role == "assistant" {
					role = "assistant"
				}
				sb.WriteString(fmt.Sprintf("[%s]: %s\n", role, b.Text))
			case "tool_use":
				sb.WriteString(fmt.Sprintf("[tool_call: %s]\n", b.Name))
			case "tool_result":
				result := toolResultSnippet(b, 200)
				if result != "" {
					sb.WriteString(fmt.Sprintf("[tool_result → %s]\n", result))
				}
			}
		}
	}

	return sb.String()
}

func toolResultSnippet(b bs.ContentBlock, maxLen int) string {
	var s string
	switch v := b.Content.(type) {
	case string:
		s = v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		s = string(data)
	}

	s = strings.TrimSpace(s)
	if len([]rune(s)) > maxLen {
		return string([]rune(s)[:maxLen]) + "…"
	}
	return s
}
