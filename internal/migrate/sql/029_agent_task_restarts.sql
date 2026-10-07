-- A restart is a new execution. Preserve the source graph, artifacts and
-- delivery receipts; repeated restart requests return the same child task.
CREATE TABLE IF NOT EXISTS agent_task_restarts (
 source_task_id uuid PRIMARY KEY REFERENCES agent_tasks(id),
 new_task_id uuid NOT NULL UNIQUE REFERENCES agent_tasks(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK (source_task_id <> new_task_id)
);
