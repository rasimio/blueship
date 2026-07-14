package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

func recordDirectLLMUsage(ctx context.Context, logger *slog.Logger, deps core.AgentDeps, sessionID, role string, req core.CompletionRequest, resp *core.CompletionResponse, started time.Time) {
	if deps.Store == nil || sessionID == "" || resp == nil {
		return
	}
	provider, model := splitProviderModel(req.Model)
	maxContext := req.ContextWindow
	if maxContext == 0 && deps.Config != nil {
		maxContext = deps.Config.Limits.MaxContext
	}
	record := core.LLMUsageRecord{
		SessionID:                   sessionID,
		Role:                        role,
		Provider:                    provider,
		Model:                       model,
		InputTokens:                 resp.Usage.InputTokens,
		OutputTokens:                resp.Usage.OutputTokens,
		CacheCreationTokens:         resp.Usage.CacheCreationTokens,
		CacheReadTokens:             resp.Usage.CacheReadTokens,
		StopReason:                  resp.StopReason,
		MessageCount:                len(req.Messages),
		ToolCount:                   len(req.Tools),
		BaseSystemTokensEstimate:    estimateTextTokens(req.System),
		SystemTokensEstimate:        estimateTextTokens(req.System),
		ToolSchemaTokensEstimate:    estimateToolSchemaTokens(req.Tools),
		DialogMessageTokensEstimate: estimateMessagesTokens(req.Messages),
		MessageTokensEstimate:       estimateMessagesTokens(req.Messages),
		MessageBudget:               req.ContextWindow,
		MessageBudgetSource:         "completion_request.context_window",
		MaxContext:                  maxContext,
		LatencyMS:                   int(time.Since(started).Milliseconds()),
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := deps.Store.RecordLLMUsage(pctx, record); err != nil && logger != nil {
		logger.Warn("record direct llm usage failed", "role", role, "session_id", sessionID, "error", err)
	}
}

func splitProviderModel(model string) (provider, name string) {
	if p, n, ok := strings.Cut(model, ":"); ok {
		return p, n
	}
	return "", model
}

func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	return len([]rune(s)) / 3
}

func estimateToolSchemaTokens(tools []core.ToolDefinition) int {
	if len(tools) == 0 {
		return 0
	}
	data, _ := json.Marshal(tools)
	return len(data) / 3
}

func estimateMessagesTokens(messages []core.Message) int {
	total := 0
	for _, msg := range messages {
		total += core.EstimateTokens(core.NormalizeContent(msg.Content))
	}
	return total
}
