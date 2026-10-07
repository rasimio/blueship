-- Persisted work graph. Only a verified result releases dependent steps.
CREATE TABLE IF NOT EXISTS agent_task_steps (
 task_id uuid NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
 step_id text NOT NULL,
 goal text NOT NULL CHECK (btrim(goal) <> ''),
 acceptance text NOT NULL CHECK (btrim(acceptance) <> ''),
 kind text NOT NULL CHECK (kind IN ('read','action','finalize')),
 tools text[] NOT NULL DEFAULT '{}',
 dependencies text[] NOT NULL DEFAULT '{}',
 status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','done','blocked','reconciliation')),
 result text NOT NULL DEFAULT '',
 checkpoint jsonb NOT NULL DEFAULT '{}',
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 last_error text NOT NULL DEFAULT '',
 attempts integer NOT NULL DEFAULT 0,
 max_attempts integer NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 10),
 run_id uuid,
 lease_until timestamptz,
 heartbeat_at timestamptz,
 started_at timestamptz,
 completed_at timestamptz,
 estimated_ms bigint CHECK (estimated_ms > 0),
 PRIMARY KEY(task_id,step_id)
);
CREATE INDEX IF NOT EXISTS agent_task_steps_ready ON agent_task_steps(task_id,status);
