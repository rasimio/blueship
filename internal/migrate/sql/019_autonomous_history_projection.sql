-- Durable post-send projection for assistant-initiated chat turns.
--
-- Telegram delivery and PostgreSQL cannot share one transaction. Once a
-- notification receipt is journaled as sent, this nullable marker turns the
-- journal row into a tiny idempotent outbox until its exact assistant message
-- has also reached the shared chat history.

ALTER TABLE agent_task_notification_attempts
    ADD COLUMN IF NOT EXISTS autonomous_history_projected_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_agent_task_notifications_autonomous_history
    ON agent_task_notification_attempts(resolved_at, id)
    WHERE state = 'sent'
      AND autonomous_history_projected_at IS NULL
      AND message_text LIKE '[blueship:autonomous-turn:v1]%';
