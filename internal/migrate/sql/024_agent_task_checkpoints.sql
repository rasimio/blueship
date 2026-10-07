-- Latest recoverable state, written during an iteration rather than only at
-- its successful end. The task-row claim fences late writes after recovery.
CREATE TABLE IF NOT EXISTS agent_task_checkpoints (
    task_id UUID PRIMARY KEY REFERENCES agent_tasks(id) ON DELETE CASCADE,
    claim_started_at TIMESTAMPTZ NOT NULL,
    iteration INTEGER NOT NULL,
    session_id TEXT NOT NULL,
    phase TEXT NOT NULL,
    output TEXT NOT NULL DEFAULT '',
    progress JSONB NOT NULL DEFAULT '{}',
    tool_calls JSONB NOT NULL DEFAULT '[]',
    receipts JSONB NOT NULL DEFAULT '[]',
    pending_tool JSONB,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
