-- Context-window and token-budget observability.

CREATE TABLE IF NOT EXISTS llm_usage (
    id                              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),
    session_id                      UUID REFERENCES chat_sessions(id) ON DELETE SET NULL,
    user_id                         UUID,
    soul_id                         UUID,
    source                          TEXT,
    role                            TEXT NOT NULL,
    provider                        TEXT,
    model                           TEXT NOT NULL,
    input_tokens                    INT NOT NULL DEFAULT 0,
    output_tokens                   INT NOT NULL DEFAULT 0,
    cache_creation_tokens           INT NOT NULL DEFAULT 0,
    cache_read_tokens               INT NOT NULL DEFAULT 0,
    stop_reason                     TEXT,
    message_count                   INT NOT NULL DEFAULT 0,
    tool_count                      INT NOT NULL DEFAULT 0,
    system_tokens_estimate          INT NOT NULL DEFAULT 0,
    tool_schema_tokens_estimate     INT NOT NULL DEFAULT 0,
    message_tokens_estimate         INT NOT NULL DEFAULT 0,
    injected_context_tokens_estimate INT NOT NULL DEFAULT 0,
    message_budget                  INT NOT NULL DEFAULT 0,
    max_context                     INT NOT NULL DEFAULT 0,
    deep_context                    BOOLEAN NOT NULL DEFAULT false,
    latency_ms                      INT NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_llm_usage_created_at ON llm_usage(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_llm_usage_user_day ON llm_usage(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_llm_usage_role_day ON llm_usage(role, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_llm_usage_session ON llm_usage(session_id, created_at DESC);

CREATE OR REPLACE VIEW llm_usage_daily AS
SELECT
    date_trunc('day', created_at)::date AS day,
    user_id,
    soul_id,
    source,
    role,
    provider,
    model,
    count(*) AS calls,
    sum(input_tokens) AS input_tokens,
    sum(output_tokens) AS output_tokens,
    sum(cache_creation_tokens) AS cache_creation_tokens,
    sum(cache_read_tokens) AS cache_read_tokens,
    sum(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens) AS total_tokens,
    sum(latency_ms) AS latency_ms,
    max(message_budget) AS max_message_budget,
    max(max_context) AS max_context,
    bool_or(deep_context) AS used_deep_context
FROM llm_usage
GROUP BY 1, 2, 3, 4, 5, 6, 7;

CREATE TABLE IF NOT EXISTS chat_session_summaries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id      UUID NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    user_id         UUID,
    soul_id         UUID,
    source          TEXT,
    to_message_id   UUID REFERENCES chat_messages(id) ON DELETE SET NULL,
    message_count   INT NOT NULL DEFAULT 0,
    token_count     INT NOT NULL DEFAULT 0,
    summary         TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_chat_session_summaries_session_created
    ON chat_session_summaries(session_id, created_at DESC);
