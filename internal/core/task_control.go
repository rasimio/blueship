package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// TaskController is the owner-scoped lifecycle surface for host commands/API.
// Localized finalization messages remain supplied by the host.
type TaskController struct {
	db     *sqlx.DB
	schema string
}

func NewTaskController(db *sqlx.DB) *TaskController { return &TaskController{db: db} }
func NewTaskControllerInSchema(db *sqlx.DB, schema string) *TaskController {
	return &TaskController{db: db, schema: schema}
}

// Cancel serializes ownership validation with finalization. A terminal report
// is never overwritten. Recurring schedules are disabled without finalizing an
// occurrence; one-shot work retains its draft and immutable cancellation result.
func (c *TaskController) Cancel(ctx context.Context, userID, soulID, id uuid.UUID, emptyBody string) (bool, error) {
	if userID == uuid.Nil || soulID == uuid.Nil || id == uuid.Nil {
		return false, sql.ErrNoRows
	}
	tx, err := c.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var task AgentTask
	if err := tx.GetContext(ctx, &task, `SELECT * FROM agent_tasks WHERE id=$1 AND user_id=$2 AND soul_id=$3 FOR UPDATE`, id, userID, soulID); err != nil {
		return false, err
	}
	changed := false
	if task.Schedule != nil || task.Cadence != nil || task.Strategy == StrategyRecurring {
		if task.Status != "canceled" {
			if _, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET status='canceled' WHERE id=$1`, id); err != nil {
				return false, err
			}
			changed = true
		}
	} else {
		_, changed, err = finalizeTaskTx(ctx, tx, id, TaskFinalization{Outcome: "cancelled", Reason: "cancelled_by_user", EmptyBody: emptyBody})
		if err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

// ErrTaskActive rejects hiding work that is still running; cancel it first.
var ErrTaskActive = errors.New("task is still active")

// Dismiss hides one finished one-shot task from status lists. The task, its
// result and lookup by ID remain.
func (c *TaskController) Dismiss(ctx context.Context, userID, soulID, id uuid.UUID) (bool, error) {
	if userID == uuid.Nil || soulID == uuid.Nil || id == uuid.Nil {
		return false, sql.ErrNoRows
	}
	tx, err := c.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var status string
	if err := tx.GetContext(ctx, &status, `SELECT status FROM agent_tasks WHERE id=$1 AND user_id=$2 AND soul_id=$3 AND schedule IS NULL AND cadence IS NULL AND strategy <> 'recurring'`, id, userID, soulID); err != nil {
		return false, err
	}
	if status != "done" && status != "failed" && status != "canceled" {
		return false, ErrTaskActive
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO agent_task_dismissals(task_id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, tx.Commit()
}

// DismissFinished hides every finished one-shot task of this owner and
// assistant from status lists. Active tasks are untouched.
func (c *TaskController) DismissFinished(ctx context.Context, userID, soulID uuid.UUID) (int, error) {
	if userID == uuid.Nil || soulID == uuid.Nil {
		return 0, sql.ErrNoRows
	}
	tx, err := c.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO agent_task_dismissals(task_id)
  SELECT id FROM agent_tasks WHERE user_id=$1 AND soul_id=$2 AND status IN ('done','failed','canceled')
   AND schedule IS NULL AND cadence IS NULL AND strategy <> 'recurring'
  ON CONFLICT DO NOTHING`, userID, soulID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}

func (c *TaskController) begin(ctx context.Context) (*sqlx.Tx, error) {
	tx, err := c.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if c.schema != "" {
		if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+pq.QuoteIdentifier(c.schema)+`,pg_catalog`); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	return tx, nil
}

// ResolveOwned resolves identifiers only inside the caller's tenant. Prefix
// ambiguity outside that scope must neither reveal nor hide another task.
func (s *AgentTaskStore) ResolveOwned(ctx context.Context, userID, soulID uuid.UUID, raw string) (AgentTask, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if userID == uuid.Nil || soulID == uuid.Nil || !taskStatusID.MatchString(raw) {
		return AgentTask{}, sql.ErrNoRows
	}
	var rows []AgentTask
	if err := s.db.SelectContext(ctx, &rows, `SELECT * FROM agent_tasks WHERE user_id=$1 AND soul_id=$2 AND id::text LIKE $3 ORDER BY id LIMIT 2`, userID, soulID, raw+"%"); err != nil {
		return AgentTask{}, err
	}
	if len(rows) != 1 {
		return AgentTask{}, sql.ErrNoRows
	}
	return rows[0], nil
}
