-- 006_agent_tasks_cadence.sql
-- Adds a per-task minimum interval between iterations for non-recurring
-- (strategy=direct/structured/delegate) tasks. Without this the
-- scheduler ticks every minute and burns iterations on long-window
-- monitors that should fire hourly or rarer. NULL = no rate limit.
ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS cadence TEXT;
