CREATE TABLE IF NOT EXISTS agent_task_runs (
 task_id uuid PRIMARY KEY REFERENCES agent_tasks(id) ON DELETE CASCADE,
 run_id uuid NOT NULL,
 lease_until timestamptz NOT NULL,
 heartbeat_at timestamptz NOT NULL,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 started_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS agent_task_runs_due ON agent_task_runs(next_attempt_at,lease_until);
