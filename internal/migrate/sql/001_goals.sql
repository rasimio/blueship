-- ============================================================
-- Goals: first-class primitive for long-running autonomous tasks.
-- Separate from agent_tasks (which stays for recurring scheduled
-- jobs like heartbeat / inner-thought / session-summary).
-- A goal has a strategy (direct / structured / delegate) that
-- determines how the runtime executes it.
-- ============================================================
CREATE TABLE IF NOT EXISTS goals (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL,
    title           TEXT NOT NULL,
    description     TEXT,

    -- strategy determines how the goal is executed.
    --   direct     — single-loop LLM with full tool registry, no pre-planned steps
    --   structured — LLM generates JSON plan up front; executor interprets steps
    --   delegate   — goal handed to a peer agent's Ship; this Ship just tracks milestones
    strategy        TEXT NOT NULL DEFAULT 'structured'
                    CHECK (strategy IN ('direct', 'structured', 'delegate')),

    -- For delegate strategy: target peer agent_id (FleetID). NULL for direct/structured.
    delegate_to     TEXT,

    -- Goal creator's config: plan_template for structured, session seed for direct, etc.
    config          JSONB NOT NULL DEFAULT '{}',

    -- Tool allow-list for this goal. NULL = full registry available.
    tools           TEXT[],

    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'running', 'paused', 'done', 'failed', 'canceled')),

    -- Runtime state owned by the strategy runner. Shape depends on strategy.
    progress        JSONB NOT NULL DEFAULT '{}',

    result          TEXT,
    error_message   TEXT,

    iteration       INT NOT NULL DEFAULT 0,
    max_iterations  INT NOT NULL DEFAULT 20,

    last_run_at     TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Conversation session (for compaction / history queries).
    session_id      TEXT
);

CREATE INDEX IF NOT EXISTS idx_goals_pending  ON goals(created_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_goals_running  ON goals(last_run_at) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_goals_paused   ON goals(last_run_at) WHERE status = 'paused';
CREATE INDEX IF NOT EXISTS idx_goals_user     ON goals(user_id, status);
CREATE INDEX IF NOT EXISTS idx_goals_delegate ON goals(delegate_to, status)
    WHERE delegate_to IS NOT NULL AND status IN ('running', 'paused');
