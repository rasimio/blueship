-- Prompt-layout observability for Cortex/tool-loop tuning.

ALTER TABLE llm_usage
    ADD COLUMN IF NOT EXISTS base_system_tokens_estimate INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS dialog_message_tokens_estimate INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS scratchpad_tokens_estimate INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS turn_context_tokens_estimate INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS message_budget_source TEXT NOT NULL DEFAULT '';

UPDATE llm_usage
SET base_system_tokens_estimate = system_tokens_estimate
WHERE base_system_tokens_estimate = 0
  AND system_tokens_estimate > 0;

UPDATE llm_usage
SET dialog_message_tokens_estimate = message_tokens_estimate
WHERE dialog_message_tokens_estimate = 0
  AND scratchpad_tokens_estimate = 0
  AND message_tokens_estimate > 0;

CREATE OR REPLACE VIEW llm_usage_prompt_layout_recent AS
SELECT
    created_at,
    session_id,
    user_id,
    soul_id,
    source,
    role,
    provider,
    model,
    input_tokens,
    output_tokens,
    cache_creation_tokens,
    cache_read_tokens,
    stop_reason,
    message_count,
    tool_count,
    message_budget,
    message_budget_source,
    max_context,
    base_system_tokens_estimate,
    turn_context_tokens_estimate,
    system_tokens_estimate,
    tool_schema_tokens_estimate,
    dialog_message_tokens_estimate,
    scratchpad_tokens_estimate,
    message_tokens_estimate,
    injected_context_tokens_estimate,
    system_tokens_estimate + tool_schema_tokens_estimate + message_tokens_estimate AS prompt_tokens_estimate,
    latency_ms
FROM llm_usage
ORDER BY created_at DESC;
