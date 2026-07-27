-- BlueShip init.sql — complete schema for fresh installations.
-- Replaces incremental migrations 001-014.
-- Run once on a new database with SET search_path TO blueship,public;

-- ============================================================
-- User profiles
-- ============================================================
CREATE TABLE IF NOT EXISTS user_profiles (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id      TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    trust_level  TEXT NOT NULL DEFAULT 'new',
    bio          JSONB DEFAULT '{}'::jsonb,
    preferences  JSONB DEFAULT '{}'::jsonb,
    timezone     TEXT DEFAULT 'Europe/Moscow',
    is_owner     BOOLEAN DEFAULT false,
    created_at   TIMESTAMPTZ DEFAULT now(),
    updated_at   TIMESTAMPTZ DEFAULT now()
);

-- ============================================================
-- Chat sessions & messages
-- ============================================================
CREATE TABLE IF NOT EXISTS chat_sessions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           UUID NOT NULL,
    title             TEXT,
    model             TEXT NOT NULL,
    system_prompt_hash TEXT,
    token_count       INT DEFAULT 0,
    message_count     INT DEFAULT 0,
    compact_summary   TEXT,
    previous_id       UUID REFERENCES chat_sessions(id),
    source            TEXT NOT NULL DEFAULT 'chat',
    source_id         UUID,
    active            BOOLEAN DEFAULT true,
    created_at        TIMESTAMPTZ DEFAULT NOW(),
    updated_at        TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_chat_sessions_user_active ON chat_sessions(user_id, active, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_chat_sessions_previous ON chat_sessions(previous_id) WHERE previous_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_chat_sessions_source ON chat_sessions(source) WHERE source <> 'chat';
CREATE INDEX IF NOT EXISTS idx_chat_sessions_source_id ON chat_sessions(source_id) WHERE source_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS chat_messages (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id     UUID NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    role           TEXT NOT NULL,
    content        JSONB NOT NULL,
    tool_use_id    TEXT,
    token_estimate INT DEFAULT 0,
    created_at     TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_chat_messages_session ON chat_messages(session_id, created_at);

-- ============================================================
-- Model configuration
-- ============================================================
CREATE TABLE IF NOT EXISTS available_models (
    provider     TEXT NOT NULL,
    name         TEXT NOT NULL,
    display_name TEXT,
    capabilities TEXT[] DEFAULT '{}',
    PRIMARY KEY (provider, name)
);

CREATE TABLE IF NOT EXISTS model_config (
    role            TEXT PRIMARY KEY,
    provider        TEXT NOT NULL,
    model_name      TEXT NOT NULL,
    thinking_budget INT DEFAULT 0,
    max_tokens      INT DEFAULT 8192,
    context_window  INT DEFAULT 0,
    message_budget  INT DEFAULT 0,
    temperature     FLOAT DEFAULT 0.7,
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

-- ============================================================
-- Agent tasks (background jobs & recurring tasks)
-- ============================================================
CREATE TABLE IF NOT EXISTS agent_tasks (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID NOT NULL,
    title          TEXT NOT NULL,
    description    TEXT,
    handler        TEXT NOT NULL,
    config         JSONB DEFAULT '{}',
    tools          TEXT[],
    schedule       TEXT,
    deadline       TIMESTAMPTZ,
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','running','paused','done','failed')),
    progress       JSONB DEFAULT '{}',
    result         TEXT,
    error_message  TEXT,
    iteration      INT NOT NULL DEFAULT 0,
    max_iterations INT NOT NULL DEFAULT 10,
    last_run_at    TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_tasks_pending ON agent_tasks(created_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_agent_tasks_running ON agent_tasks(last_run_at) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_agent_tasks_user ON agent_tasks(user_id, status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_tasks_recurring ON agent_tasks(user_id, handler) WHERE schedule IS NOT NULL AND status != 'failed';

CREATE TABLE IF NOT EXISTS agent_task_deliveries (
    task_id      UUID NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
    input_id     TEXT NOT NULL,
    item_key     TEXT NOT NULL CHECK (octet_length(item_key) <= 512),
    delivered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (task_id, input_id, item_key)
);

-- At-most-once notification journal for keyed agent-task deliveries.
--
-- The reservation row is created before the transport is called and is never
-- released. Explicit retryable failures resend this exact immutable intent;
-- a crash or ambiguous timeout leaves it dispatching/uncertain and permanently
-- suppresses the occurrence instead of risking a duplicate generated message.

CREATE TABLE IF NOT EXISTS agent_task_notification_attempts (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id        UUID NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
    user_id        UUID NOT NULL,
    occurrence_key TEXT NOT NULL
                   CHECK (octet_length(occurrence_key) BETWEEN 1 AND 512),
    message_text   TEXT NOT NULL CHECK (btrim(message_text) <> ''),
    state          TEXT NOT NULL DEFAULT 'dispatching'
                   CHECK (state IN ('dispatching', 'retryable', 'sent', 'uncertain', 'rejected')),
    receipt        JSONB,
    error_message  TEXT,
    attempt_count  INTEGER NOT NULL DEFAULT 1 CHECK (attempt_count >= 1),
    next_attempt_at TIMESTAMPTZ,
    last_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at    TIMESTAMPTZ,

    CONSTRAINT agent_task_notification_attempts_occurrence_unique
        UNIQUE (task_id, occurrence_key),
    CONSTRAINT agent_task_notification_attempts_id_task_unique
        UNIQUE (id, task_id),
    CONSTRAINT agent_task_notification_attempts_state_shape_check CHECK (
        (state = 'dispatching' AND receipt IS NULL AND error_message IS NULL AND next_attempt_at IS NULL AND resolved_at IS NULL)
        OR
        (state = 'retryable' AND receipt IS NULL AND error_message IS NOT NULL AND next_attempt_at IS NOT NULL AND resolved_at IS NULL)
        OR
        (state = 'sent' AND receipt IS NOT NULL AND error_message IS NULL AND next_attempt_at IS NULL AND resolved_at IS NOT NULL)
        OR
        (state IN ('uncertain', 'rejected') AND receipt IS NULL AND error_message IS NOT NULL AND next_attempt_at IS NULL AND resolved_at IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_agent_task_notification_attempts_retryable
    ON agent_task_notification_attempts(next_attempt_at, created_at, id)
    WHERE state = 'retryable';

-- One immutable notification may represent several integration occurrences.
-- The task-scoped unique constraint is the at-most-once reservation fence:
-- every state, including retryable/rejected, suppresses task regeneration.
CREATE TABLE IF NOT EXISTS agent_task_notification_attempt_items (
    attempt_id UUID NOT NULL,
    task_id    UUID NOT NULL,
    input_id   TEXT NOT NULL CHECK (octet_length(input_id) BETWEEN 1 AND 64),
    item_key   TEXT NOT NULL CHECK (octet_length(item_key) BETWEEN 1 AND 512),

    PRIMARY KEY (attempt_id, input_id, item_key),
    CONSTRAINT agent_task_notification_attempt_items_occurrence_unique
        UNIQUE (task_id, input_id, item_key),
    CONSTRAINT agent_task_notification_attempt_items_attempt_fk
        FOREIGN KEY (attempt_id, task_id)
        REFERENCES agent_task_notification_attempts(id, task_id)
        ON DELETE CASCADE
);

-- Durable post-send projection for assistant-initiated chat turns.
--
-- Telegram delivery and PostgreSQL cannot share one transaction. Once a
-- notification receipt is journaled as sent, this nullable marker turns the
-- journal row into a tiny idempotent outbox until its exact assistant message
-- has also reached the shared chat history.

ALTER TABLE agent_task_notification_attempts
    ADD COLUMN IF NOT EXISTS autonomous_history_projected_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_agent_task_notifications_autonomous_history
    ON agent_task_notification_attempts(resolved_at, id)
    WHERE state = 'sent'
      AND autonomous_history_projected_at IS NULL
      AND message_text LIKE '[blueship:autonomous-turn:v1]%';

-- ============================================================
-- A2A (Agent-to-Agent) protocol — universal tool bus
-- Each ship exposes selected local tools to other ships and imports
-- interesting tools from its peers. RemoteTool handlers dispatch via
-- HTTP; the cortex sees them as ordinary tools.
-- History of every inter-ship call (both in and out) is append-only
-- persisted in a2a_calls + a2a_events for audit, debug, and replay.
-- ============================================================
CREATE TABLE IF NOT EXISTS a2a_peers (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL UNIQUE,     -- stable identifier e.g. "liya"
    base_url      TEXT NOT NULL,            -- e.g. "http://localhost:8090"
    auth_token    TEXT,                     -- shared secret
    agent_card    JSONB,                    -- cached /.well-known/agent response
    card_fetched_at TIMESTAMPTZ,
    last_seen_at  TIMESTAMPTZ,
    enabled       BOOLEAN NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_a2a_peers_enabled ON a2a_peers(name) WHERE enabled = true;

-- Remote tools imported from peers; registered into the local ToolRegistry
-- as RemoteTool handlers at startup.
CREATE TABLE IF NOT EXISTS a2a_remote_tools (
    peer_id       UUID NOT NULL REFERENCES a2a_peers(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    mode          TEXT NOT NULL,
    description   TEXT NOT NULL,
    schema        JSONB NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (peer_id, name)
);

CREATE INDEX IF NOT EXISTS idx_a2a_remote_tools_name ON a2a_remote_tools(name);

-- Every inter-ship tool invocation, one row per call (both directions).
-- 'direction' is relative to THIS ship:
--   out = this ship called a peer's tool
--   in  = a peer called a local exposed tool
CREATE TABLE IF NOT EXISTS a2a_calls (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    peer_id        UUID REFERENCES a2a_peers(id) ON DELETE SET NULL,
    peer_name      TEXT NOT NULL,            -- denormalised for logs
    direction      TEXT NOT NULL,            -- 'out' | 'in'
    tool_name      TEXT NOT NULL,
    mode           TEXT NOT NULL,            -- 'sync' | 'async'
    correlation_id TEXT,
    input          JSONB,
    output         JSONB,
    error          TEXT,
    state          TEXT NOT NULL,            -- 'pending' | 'running' | 'done' | 'failed' | 'canceled'
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_a2a_calls_peer ON a2a_calls(peer_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_a2a_calls_active ON a2a_calls(state, created_at) WHERE state IN ('pending','running');
CREATE INDEX IF NOT EXISTS idx_a2a_calls_corr ON a2a_calls(correlation_id) WHERE correlation_id IS NOT NULL;

-- Streamed events per async call. Long-poll / SSE on the server reads by
-- (call_id, seq > since); terminal events carry is_final = true.
CREATE TABLE IF NOT EXISTS a2a_events (
    id          BIGSERIAL PRIMARY KEY,
    call_id     UUID NOT NULL REFERENCES a2a_calls(id) ON DELETE CASCADE,
    seq         INT NOT NULL,                -- monotonic per call
    type        TEXT NOT NULL,               -- 'state_change' | 'output' | 'log' | 'terminal'
    payload     JSONB,
    is_final    BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (call_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_a2a_events_call ON a2a_events(call_id, seq);
