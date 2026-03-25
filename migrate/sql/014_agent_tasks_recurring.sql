CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_tasks_recurring
    ON agent_tasks (user_id, handler) WHERE schedule IS NOT NULL AND status != 'failed';
