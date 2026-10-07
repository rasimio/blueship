package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// TaskSubmission decouples producing a report from checking it. A reviewer
// outage retries this exact saved candidate, never the research or its effects.
type TaskSubmission struct {
	Output            string          `db:"output"`
	Notify            string          `db:"notify"`
	Progress          json.RawMessage `db:"progress"`
	ToolCalls         json.RawMessage `db:"tool_calls"`
	PendingDeliveries json.RawMessage `db:"pending_deliveries"`
	ReviewAttempts    int             `db:"review_attempts"`
	NextReviewAt      time.Time       `db:"next_review_at"`
}

func (s *AgentTaskStore) TaskSubmission(ctx context.Context, id uuid.UUID) (*TaskSubmission, error) {
	var submission TaskSubmission
	err := s.db.GetContext(ctx, &submission, `SELECT output,notify,progress,tool_calls,pending_deliveries,review_attempts,next_review_at FROM agent_task_submissions WHERE task_id=$1`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &submission, err
}

func (s *AgentTaskStore) SaveTaskSubmission(ctx context.Context, id uuid.UUID, claim time.Time, result IterationResult) error {
	tools := result.ToolCallsJSON
	if len(tools) == 0 {
		tools = json.RawMessage(`[]`)
	}
	refs, _ := json.Marshal(result.PendingDeliveries)
	res, err := s.db.ExecContext(ctx, `WITH claimed AS (
		SELECT id FROM agent_tasks WHERE id=$1 AND status='running' AND last_run_at=$2 FOR UPDATE
	) INSERT INTO agent_task_submissions (task_id,output,notify,progress,tool_calls,pending_deliveries)
	SELECT id,$3,$4,$5,$6,$7 FROM claimed
	ON CONFLICT (task_id) DO UPDATE SET output=EXCLUDED.output,notify=EXCLUDED.notify,
	progress=EXCLUDED.progress,tool_calls=EXCLUDED.tool_calls,pending_deliveries=EXCLUDED.pending_deliveries,review_attempts=0,next_review_at=now(),last_error=''`,
		id, claim, result.Output, result.Notify, jsonbObject(result.Progress), tools, refs)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrTaskClaimLost
	}
	return err
}

func (s *AgentTaskStore) DeferTaskVerification(ctx context.Context, id uuid.UUID, claim time.Time, reason string, next time.Time) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET status='pending',progress=COALESCE(progress,'{}'::jsonb) || '{"phase":"verification_wait"}'::jsonb WHERE id=$1 AND status='running' AND last_run_at=$2`, id, claim)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrTaskClaimLost
	}
	res, err = tx.ExecContext(ctx, `UPDATE agent_task_submissions SET review_attempts=review_attempts+1,next_review_at=$2,last_error=$3 WHERE task_id=$1`, id, next, reason)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// RejectTaskSubmission releases the candidate and schedules its repair atomically.
// A stale reviewer must not delete a replacement worker's report or progress.
func (s *AgentTaskStore) RejectTaskSubmission(ctx context.Context, id uuid.UUID, claim time.Time, progress json.RawMessage, recheckURLs []string) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if recheckURLs == nil {
		recheckURLs = []string{}
	}
	res, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET progress=$3, status='pending', iteration=iteration+1, required_recheck_urls=$4
 WHERE id=$1 AND status='running' AND last_run_at=$2`, id, claim, jsonbObject(progress), pq.Array(recheckURLs))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTaskClaimLost
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_task_submissions WHERE task_id=$1`, id); err != nil {
		return err
	}
	return tx.Commit()
}
