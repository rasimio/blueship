package core

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Dispatch and receipt persistence have separate 10-second budgets. An old
// dispatch without a receipt is unknown, including after a process crash.
// This is a read projection only: a late provider ACK can still be recorded.
const notificationStatusStaleAfter = time.Minute

type taskResultDelivery struct {
	TaskID        uuid.UUID  `db:"task_id"`
	State         string     `db:"state"`
	NextAttemptAt *time.Time `db:"next_attempt_at"`
	ConfirmedAt   *time.Time `db:"confirmed_at"`
}

// Read only the latest artifact's notification. A sent progress update or an
// older artifact is not evidence that the current report was delivered.
func (r *TaskStatusReader) readResultDeliveries(ctx context.Context, user, soul uuid.UUID, ids []string) (map[uuid.UUID]taskResultDelivery, error) {
	result := map[uuid.UUID]taskResultDelivery{}
	if len(ids) == 0 {
		return result, nil
	}
	var rows []taskResultDelivery
	err := r.db.SelectContext(ctx, &rows, r.qualify(`SELECT t.id AS task_id,
 CASE WHEN n.id IS NULL THEN 'not_requested'
 WHEN n.state='retryable' AND n.attempt_count=0 THEN 'pending'
 WHEN n.state='retryable' THEN 'retrying'
 WHEN n.state='dispatching' AND n.last_attempt_at < clock_timestamp()-$4*interval '1 second' THEN 'uncertain'
 WHEN n.state='dispatching' THEN 'sending'
 WHEN n.state='rejected' THEN 'undeliverable'
 ELSE n.state END AS state,
 CASE WHEN n.state='retryable' THEN n.next_attempt_at END AS next_attempt_at,
 CASE WHEN n.state='sent' THEN n.resolved_at END AS confirmed_at
 FROM agent_tasks t
 JOIN LATERAL (SELECT version FROM agent_task_artifacts a WHERE a.task_id=t.id ORDER BY version DESC LIMIT 1) a ON true
 LEFT JOIN agent_task_notification_attempt_items i ON i.task_id=t.id AND i.input_id='task_result' AND i.item_key='version:' || a.version::text
 LEFT JOIN agent_task_notification_attempts n ON n.id=i.attempt_id AND n.task_id=t.id AND n.user_id=t.user_id
 WHERE t.user_id=$1 AND t.soul_id=$2 AND t.id=ANY($3::uuid[])`), user, soul, pq.Array(ids), notificationStatusStaleAfter.Seconds())
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.TaskID] = row
	}
	return result, nil
}
