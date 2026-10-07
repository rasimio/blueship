-- Finished tasks the owner removed from /status lists. A separate table, not a
-- column: older binaries scan agent_tasks.* and must keep working on rollback.
CREATE TABLE IF NOT EXISTS agent_task_dismissals (
  task_id uuid PRIMARY KEY REFERENCES agent_tasks(id) ON DELETE CASCADE,
  dismissed_at timestamptz NOT NULL DEFAULT now()
);
