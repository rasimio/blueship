package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// TaskCheckpoint is the recoverable state of an in-flight iteration. Output
// is a draft, never proof of acceptance. PendingTool denotes an action whose
// result may need reconciliation after a worker dies.
type TaskCheckpoint struct {
	Iteration   int             `db:"iteration" json:"iteration"`
	SessionID   string          `db:"session_id" json:"session_id"`
	Phase       string          `db:"phase" json:"phase"`
	Output      string          `db:"output" json:"output"`
	Progress    json.RawMessage `db:"progress" json:"progress"`
	ToolCalls   json.RawMessage `db:"tool_calls" json:"tool_calls"`
	Receipts    json.RawMessage `db:"receipts" json:"receipts"`
	PendingTool json.RawMessage `db:"pending_tool" json:"pending_tool,omitempty"`
	UpdatedAt   time.Time       `db:"updated_at" json:"updated_at"`
}

var ErrTaskClaimLost = errors.New("task execution claim is no longer current")

// CheckpointTask serializes with cancellation/finalization and requires the
// exact claim timestamp. Recovery that replaces the claim fences this writer,
// including a late write using a detached persistence context.
func (s *AgentTaskStore) CheckpointTask(ctx context.Context, id uuid.UUID, claimStartedAt time.Time, cp TaskCheckpoint) error {
	if cp.SessionID == "" || cp.Phase == "" || cp.Iteration <= 0 || claimStartedAt.IsZero() {
		return fmt.Errorf("invalid task checkpoint identity")
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current uuid.UUID
	err = tx.GetContext(ctx, &current, `SELECT id FROM agent_tasks
		WHERE id = $1 AND status = 'running' AND last_run_at = $2
		FOR UPDATE`, id, claimStartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTaskClaimLost
	}
	if err != nil {
		return err
	}
	if len(cp.ToolCalls) == 0 {
		cp.ToolCalls = json.RawMessage(`[]`)
	}
	if len(cp.Receipts) == 0 {
		cp.Receipts = json.RawMessage(`[]`)
	}
	cp.Progress = jsonbObject(cp.Progress)
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_task_checkpoints
		(task_id, claim_started_at, iteration, session_id, phase, output, progress, tool_calls, receipts, pending_tool)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (task_id) DO UPDATE SET
		claim_started_at=EXCLUDED.claim_started_at, iteration=EXCLUDED.iteration,
		session_id=EXCLUDED.session_id, phase=EXCLUDED.phase,
		output=CASE WHEN EXCLUDED.output <> '' THEN EXCLUDED.output ELSE agent_task_checkpoints.output END,
		progress=EXCLUDED.progress, tool_calls=EXCLUDED.tool_calls,
		receipts=EXCLUDED.receipts, pending_tool=EXCLUDED.pending_tool, updated_at=now()`,
		id, claimStartedAt, cp.Iteration, cp.SessionID, cp.Phase, cp.Output, cp.Progress, cp.ToolCalls, cp.Receipts, cp.PendingTool)
	if err != nil {
		return err
	}
	// The worker session is part of progress; task.session_id may refer to the
	// originating chat and must not be replaced with the worker session.
	_, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET progress=COALESCE(progress,'{}'::jsonb) || $2::jsonb WHERE id=$1`, id, cp.Progress)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AgentTaskStore) TaskCheckpoint(ctx context.Context, id uuid.UUID) (*TaskCheckpoint, error) {
	var cp TaskCheckpoint
	err := s.db.GetContext(ctx, &cp, `SELECT iteration, session_id, phase, output, progress,
		tool_calls, receipts, COALESCE(pending_tool,'null'::jsonb) AS pending_tool,
		updated_at FROM agent_task_checkpoints WHERE task_id=$1`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &cp, err
}
