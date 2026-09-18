package core

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

const DefaultTaskWallTimeout = 30 * time.Minute
const TaskDeadlineExceeded = "wall_clock_deadline_exceeded"

// TaskWallDeadline is an absolute budget, including queueing and pauses. A
// deliberate start_at moves the start of the budget, but retries and restarts
// never do. Periodic monitors and recurring jobs keep their own lifecycle.
func TaskWallDeadline(task AgentTask, now time.Time, limit time.Duration) *time.Time {
	if task.Schedule != nil || task.Cadence != nil || task.Strategy == StrategyRecurring {
		return task.Deadline
	}
	if limit <= 0 {
		limit = DefaultTaskWallTimeout
	}
	deadline := TaskWallStart(task, now).Add(limit)
	if task.Deadline != nil && task.Deadline.Before(deadline) {
		deadline = *task.Deadline
	}
	return &deadline
}

func TaskWallStart(task AgentTask, now time.Time) time.Time {
	start := task.CreatedAt
	if start.IsZero() {
		start = now
	}
	var cfg struct {
		StartAt string `json:"start_at"`
	}
	if json.Unmarshal(task.Config, &cfg) == nil {
		if scheduled, err := time.Parse(time.RFC3339, cfg.StartAt); err == nil && scheduled.After(start) {
			start = scheduled
		}
	}
	return start
}

// DeadlineTasks includes paused/queued tasks and expired tasks whose status
// notification has not yet been journaled (e.g. a crash after the state write).
func (s *AgentTaskStore) DeadlineTasks(ctx context.Context) ([]AgentTask, error) {
	var tasks []AgentTask
	err := s.db.SelectContext(ctx, &tasks, `
		SELECT t.* FROM agent_tasks t
		WHERE (t.status IN ('pending', 'running', 'paused') AND
		       (t.deadline IS NOT NULL OR (t.schedule IS NULL AND t.cadence IS NULL AND t.strategy <> 'recurring')))
		   OR (t.status = 'failed' AND t.error_message = $1 AND NOT EXISTS (
		       SELECT 1 FROM agent_task_notification_attempt_items n
		       WHERE n.task_id = t.id AND n.input_id = 'task_lifecycle' AND n.item_key = 'deadline'))
		ORDER BY t.created_at`, TaskDeadlineExceeded)
	return tasks, err
}

func (s *AgentTaskStore) SetTaskDeadline(ctx context.Context, id uuid.UUID, deadline time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agent_tasks SET deadline = $2
		WHERE id = $1 AND status IN ('pending', 'running', 'paused')
		  AND (deadline IS NULL OR deadline > $2)`, id, deadline)
	return err
}

// ExpireTask is terminal; late worker completion must never resurrect it.
func (s *AgentTaskStore) ExpireTask(ctx context.Context, id uuid.UUID, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE agent_tasks
		SET status = 'failed', error_message = $3, completed_at = $2
		WHERE id = $1 AND status IN ('pending', 'running', 'paused') AND deadline <= $2`, id, now, TaskDeadlineExceeded)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}
