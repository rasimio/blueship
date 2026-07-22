-- At-most-once notification journal for keyed agent-task deliveries.
--
-- The reservation row is created before the transport is called and is never
-- released. Explicit retryable failures resend this exact immutable intent;
-- a crash or ambiguous timeout leaves it dispatching/uncertain and permanently
-- suppresses the occurrence instead of risking a duplicate generated message.

CREATE TABLE IF NOT EXISTS agent_task_notification_attempts (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id        UUID NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
    user_id        UUID NOT NULL,
    occurrence_key TEXT NOT NULL
                   CHECK (octet_length(occurrence_key) BETWEEN 1 AND 512),
    message_text   TEXT NOT NULL CHECK (btrim(message_text) <> ''),
    state          TEXT NOT NULL DEFAULT 'dispatching'
                   CHECK (state IN ('dispatching', 'retryable', 'sent', 'uncertain', 'rejected')),
    receipt        JSONB,
    error_message  TEXT,
    attempt_count  INTEGER NOT NULL DEFAULT 1 CHECK (attempt_count >= 1),
    next_attempt_at TIMESTAMPTZ,
    last_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at    TIMESTAMPTZ,

    CONSTRAINT agent_task_notification_attempts_occurrence_unique
        UNIQUE (task_id, occurrence_key),
    CONSTRAINT agent_task_notification_attempts_id_task_unique
        UNIQUE (id, task_id),
    CONSTRAINT agent_task_notification_attempts_state_shape_check CHECK (
        (state = 'dispatching' AND receipt IS NULL AND error_message IS NULL AND next_attempt_at IS NULL AND resolved_at IS NULL)
        OR
        (state = 'retryable' AND receipt IS NULL AND error_message IS NOT NULL AND next_attempt_at IS NOT NULL AND resolved_at IS NULL)
        OR
        (state = 'sent' AND receipt IS NOT NULL AND error_message IS NULL AND next_attempt_at IS NULL AND resolved_at IS NOT NULL)
        OR
        (state IN ('uncertain', 'rejected') AND receipt IS NULL AND error_message IS NOT NULL AND next_attempt_at IS NULL AND resolved_at IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_agent_task_notification_attempts_retryable
    ON agent_task_notification_attempts(next_attempt_at, created_at, id)
    WHERE state = 'retryable';

-- One immutable notification may represent several integration occurrences.
-- The task-scoped unique constraint is the at-most-once reservation fence:
-- every state, including retryable/rejected, suppresses task regeneration.
CREATE TABLE IF NOT EXISTS agent_task_notification_attempt_items (
    attempt_id UUID NOT NULL,
    task_id    UUID NOT NULL,
    input_id   TEXT NOT NULL CHECK (octet_length(input_id) BETWEEN 1 AND 64),
    item_key   TEXT NOT NULL CHECK (octet_length(item_key) BETWEEN 1 AND 512),

    PRIMARY KEY (attempt_id, input_id, item_key),
    CONSTRAINT agent_task_notification_attempt_items_occurrence_unique
        UNIQUE (task_id, input_id, item_key),
    CONSTRAINT agent_task_notification_attempt_items_attempt_fk
        FOREIGN KEY (attempt_id, task_id)
        REFERENCES agent_task_notification_attempts(id, task_id)
        ON DELETE CASCADE
);
