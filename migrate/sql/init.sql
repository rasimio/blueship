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
-- System prompts
-- ============================================================
CREATE TABLE IF NOT EXISTS system_prompts (
    key        TEXT PRIMARY KEY,
    content    TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

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
    temperature     FLOAT DEFAULT 0.7,
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

-- ============================================================
-- Role-based tool assignment
-- ============================================================
CREATE TABLE IF NOT EXISTS role_tools (
    role       TEXT NOT NULL,
    tool_name  TEXT NOT NULL,
    sort_order INT DEFAULT 0,
    PRIMARY KEY (role, tool_name)
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
