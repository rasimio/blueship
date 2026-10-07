-- Existing tasks retain v1. Creation of v2 is enabled only after its dispatcher
-- and all maintenance paths are installed; the version never changes mid-task.
ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS executor_version integer NOT NULL DEFAULT 1 CHECK (executor_version IN (1,2));
CREATE INDEX IF NOT EXISTS agent_task_executor_pending ON agent_tasks(executor_version,status,created_at);
