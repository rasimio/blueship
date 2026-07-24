package agent

import (
	"context"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

func (a *Loop) recordLLMUsage(
	ctx context.Context,
	cfg RunConfig,
	model string,
	tools []bs.ToolDefinition,
	messages []bs.Message,
	baseSystem string,
	effectiveSystem string,
	tokenBudget int,
	tokenBudgetSource string,
	injectedContext string,
	dialogTokens int,
	scratchpadTokens int,
	usage bs.Usage,
	stopReason string,
	started time.Time,
) {
	if cfg.SessionID == "" {
		return
	}
	provider, modelName := splitProviderModel(model)
	maxContext := a.cfg.Limits.MaxContext
	if cfg.ContextWindow > 0 {
		maxContext = cfg.ContextWindow
	}
	record := bs.LLMUsageRecord{
		SessionID:                   cfg.SessionID,
		Role:                        cfg.Role,
		Provider:                    provider,
		Model:                       modelName,
		InputTokens:                 usage.InputTokens,
		OutputTokens:                usage.OutputTokens,
		CacheCreationTokens:         usage.CacheCreationTokens,
		CacheReadTokens:             usage.CacheReadTokens,
		StopReason:                  stopReason,
		MessageCount:                len(messages),
		ToolCount:                   len(tools),
		BaseSystemTokensEstimate:    estimateTextTokens(baseSystem),
		SystemTokensEstimate:        estimateTextTokens(effectiveSystem),
		ToolSchemaTokensEstimate:    estimateToolSchemaTokens(tools),
		DialogMessageTokensEstimate: dialogTokens,
		ScratchpadTokensEstimate:    scratchpadTokens,
		MessageTokensEstimate:       estimateMessagesTokens(messages),
		TurnContextTokensEstimate:   estimateTextTokens(injectedContext),
		InjectedContextTokens:       estimateTextTokens(injectedContext),
		MessageBudget:               tokenBudget,
		MessageBudgetSource:         tokenBudgetSource,
		MaxContext:                  maxContext,
		DeepContext:                 false,
		LatencyMS:                   int(time.Since(started).Milliseconds()),
	}
	if record.Role == "" {
		record.Role = "unknown"
	}
	if err := a.store.RecordLLMUsage(ctx, record); err != nil {
		a.logger.Warn("record llm usage failed", "error", err)
	}
}

func splitProviderModel(model string) (provider, name string) {
	if p, n, ok := strings.Cut(model, ":"); ok {
		return p, n
	}
	return "", model
}
