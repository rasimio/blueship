-- Phase B unification: agent_tasks becomes the universal task primitive.
-- Goals collapse into agent_tasks; the goals table is dropped.
--
-- Key shape change: completion is no longer iteration-bound. Tasks ship
-- with an acceptance_criteria description; the executor evaluates the
-- result against that criteria and only then transitions to status=done.
-- max_iterations remains as a runaway safety cap, not a success bar.

-- ---------------------------------------------------------------------
-- 1. Extend agent_tasks with the universal fields
-- ---------------------------------------------------------------------
ALTER TABLE agent_tasks
    ADD COLUMN IF NOT EXISTS acceptance_criteria TEXT,
    ADD COLUMN IF NOT EXISTS plan JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS use_agents TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS strategy TEXT NOT NULL DEFAULT 'recurring',
    ADD COLUMN IF NOT EXISTS delegate_to TEXT,
    ADD COLUMN IF NOT EXISTS session_id TEXT;

-- handler stays NOT NULL but goal-style rows store an empty string. The
-- scheduler dispatches by strategy first; handler only matters when
-- strategy='recurring'.

-- canceled was a goals state; agent_tasks needs to accept it too.
ALTER TABLE agent_tasks DROP CONSTRAINT IF EXISTS agent_tasks_status_check;
ALTER TABLE agent_tasks ADD CONSTRAINT agent_tasks_status_check
    CHECK (status IN ('pending','running','paused','done','failed','canceled'));

ALTER TABLE agent_tasks DROP CONSTRAINT IF EXISTS agent_tasks_strategy_check;
ALTER TABLE agent_tasks ADD CONSTRAINT agent_tasks_strategy_check
    CHECK (strategy IN ('recurring','direct','structured','delegate'));

-- ---------------------------------------------------------------------
-- 2. Migrate goals rows into agent_tasks
-- ---------------------------------------------------------------------
-- Pre-seed: ensure target ids don't already exist (idempotent re-run).
INSERT INTO agent_tasks (
    id, user_id, title, description, handler, config, tools, status,
    progress, result, error_message, iteration, max_iterations,
    last_run_at, completed_at, created_at, strategy, delegate_to, session_id, plan
)
SELECT
    g.id, g.user_id, g.title, g.description,
    '',                                    -- handler empty — strategy drives execution
    g.config, g.tools, g.status, g.progress, g.result, g.error_message,
    g.iteration, g.max_iterations, g.last_run_at, g.completed_at, g.created_at,
    g.strategy, g.delegate_to, g.session_id,
    -- structured strategy stores the plan inside progress.plan today; lift
    -- it out into the new top-level plan column where it belongs.
    COALESCE(g.progress -> 'plan', '{}'::jsonb)
FROM goals g
WHERE NOT EXISTS (SELECT 1 FROM agent_tasks t WHERE t.id = g.id);

-- ---------------------------------------------------------------------
-- 3. Drop goals
-- ---------------------------------------------------------------------
DROP TABLE IF EXISTS goals;

-- ---------------------------------------------------------------------
-- 4. Indexes for the new strategy-based dispatch path
-- ---------------------------------------------------------------------
CREATE INDEX IF NOT EXISTS idx_agent_tasks_strategy_active
    ON agent_tasks(strategy, status)
    WHERE status IN ('pending', 'running', 'paused');

CREATE INDEX IF NOT EXISTS idx_agent_tasks_delegate
    ON agent_tasks(delegate_to, status)
    WHERE delegate_to IS NOT NULL AND status IN ('running', 'paused');
