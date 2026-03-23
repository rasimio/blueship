CREATE TABLE IF NOT EXISTS agent_tasks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL,
    title           TEXT NOT NULL,
    description     TEXT,

    -- what to do
    handler         TEXT NOT NULL,
    config          JSONB DEFAULT '{}',
    tools           TEXT[],

    -- when to do it
    schedule        TEXT,
    deadline        TIMESTAMPTZ,

    -- state
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','running','paused','done','failed')),
    progress        JSONB DEFAULT '{}',
    result          TEXT,
    error_message   TEXT,

    -- metrics
    iteration       INT NOT NULL DEFAULT 0,
    max_iterations  INT NOT NULL DEFAULT 10,
    last_run_at     TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_tasks_pending
    ON agent_tasks (created_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_agent_tasks_running
    ON agent_tasks (last_run_at) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_agent_tasks_user
    ON agent_tasks (user_id, status);
