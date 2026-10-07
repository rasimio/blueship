CREATE TABLE IF NOT EXISTS agent_task_submissions (
    task_id UUID PRIMARY KEY REFERENCES agent_tasks(id) ON DELETE CASCADE,
    output TEXT NOT NULL CHECK (btrim(output) <> ''),
    notify TEXT NOT NULL DEFAULT '',
    progress JSONB NOT NULL DEFAULT '{}',
    tool_calls JSONB NOT NULL DEFAULT '[]',
    pending_deliveries JSONB NOT NULL DEFAULT '[]',
    review_attempts INTEGER NOT NULL DEFAULT 0,
    next_review_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS agent_task_artifacts (
    task_id UUID NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK (version > 0),
    outcome TEXT NOT NULL CHECK (outcome IN ('completed','partial','blocked','cancelled')),
    body TEXT NOT NULL CHECK (btrim(body) <> ''),
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, version)
);

-- Zero means an immutable outbox message has been queued but no transport
-- call has been attempted. ClaimRetryableNotification increments it to one.
ALTER TABLE agent_task_notification_attempts
    DROP CONSTRAINT IF EXISTS agent_task_notification_attempts_attempt_count_check;
ALTER TABLE agent_task_notification_attempts
    ADD CONSTRAINT agent_task_notification_attempts_attempt_count_check CHECK (attempt_count >= 0);
