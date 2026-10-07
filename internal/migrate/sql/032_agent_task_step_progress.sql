-- A lease heartbeat is not evidence of useful progress. Existing timestamps
-- are intentionally not backfilled from heartbeat_at.
ALTER TABLE agent_task_steps ADD COLUMN IF NOT EXISTS progress_at timestamptz;
