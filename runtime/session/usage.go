package session

import (
	"context"
	"fmt"

	bs "github.com/rasimio/blueship/internal/core"
)

func (s *Store) RecordLLMUsage(ctx context.Context, r bs.LLMUsageRecord) error {
	if r.SessionID == "" {
		return nil
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO llm_usage (
			session_id, user_id, soul_id, source,
			role, provider, model,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			stop_reason, message_count, tool_count,
			base_system_tokens_estimate, system_tokens_estimate, tool_schema_tokens_estimate,
			dialog_message_tokens_estimate, scratchpad_tokens_estimate,
			message_tokens_estimate, turn_context_tokens_estimate, injected_context_tokens_estimate,
			message_budget, message_budget_source, max_context, deep_context, latency_ms
		)
		SELECT
			cs.id, cs.user_id, cs.soul_id, cs.source,
			$2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11,
			$12, $13, $14,
			$15, $16,
			$17, $18, $19,
			$20, $21, $22, $23, $24
		FROM chat_sessions cs
		WHERE cs.id = $1`,
		r.SessionID,
		r.Role, r.Provider, r.Model,
		r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens,
		r.StopReason, r.MessageCount, r.ToolCount,
		r.BaseSystemTokensEstimate, r.SystemTokensEstimate, r.ToolSchemaTokensEstimate,
		r.DialogMessageTokensEstimate, r.ScratchpadTokensEstimate,
		r.MessageTokensEstimate, r.TurnContextTokensEstimate, r.InjectedContextTokens,
		r.MessageBudget, r.MessageBudgetSource, r.MaxContext, r.DeepContext, r.LatencyMS,
	)
	if err != nil {
		return fmt.Errorf("record llm usage: %w", err)
	}
	if rows, rowErr := res.RowsAffected(); rowErr == nil && rows == 0 {
		return fmt.Errorf("record llm usage: session not found: %s", r.SessionID)
	}
	return nil
}
