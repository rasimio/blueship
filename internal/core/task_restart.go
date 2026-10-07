package core

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

var ErrTaskNotTerminal = errors.New("task must be terminal before restart")
var ErrTaskRestartUncertain = errors.New("external operation requires reconciliation before restart")
var ErrTaskNotRecurring = errors.New("one-shot tasks must use restart instead of resume")

// Restart explicitly repeats a terminal one-shot task as a new execution.
// The source stays immutable. Serializing on it makes duplicate API requests
// idempotent, including a retry after the client lost the committed response.
func (c *TaskController) Restart(ctx context.Context, user, soul, id uuid.UUID, budget time.Duration) (uuid.UUID, error) {
	if user == uuid.Nil || soul == uuid.Nil || id == uuid.Nil {
		return uuid.Nil, sql.ErrNoRows
	}
	if budget <= 0 {
		budget = DefaultTaskWallTimeout
	}
	tx, err := c.db.BeginTxx(ctx, nil)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback()
	if c.schema != "" {
		if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+pq.QuoteIdentifier(c.schema)+`,pg_catalog`); err != nil {
			return uuid.Nil, err
		}
	}
	var task AgentTask
	if err := tx.GetContext(ctx, &task, `SELECT * FROM agent_tasks WHERE id=$1 AND user_id=$2 AND soul_id=$3 FOR UPDATE`, id, user, soul); err != nil {
		return uuid.Nil, err
	}
	if task.Schedule != nil || task.Cadence != nil || task.Strategy == StrategyRecurring {
		return uuid.Nil, ErrTaskNotTerminal
	}
	if task.Status != "done" && task.Status != "failed" && task.Status != "canceled" {
		return uuid.Nil, ErrTaskNotTerminal
	}
	var previous uuid.UUID
	err = tx.GetContext(ctx, &previous, `SELECT new_task_id FROM agent_task_restarts WHERE source_task_id=$1`, id)
	if err == nil {
		return previous, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, err
	}
	if task.ExecutorVersion == 2 {
		var uncertain bool
		if err := tx.GetContext(ctx, &uncertain, `SELECT EXISTS(SELECT 1 FROM agent_task_steps WHERE task_id=$1 AND kind<>'read' AND NOT(kind='finalize' AND cardinality(tools)=0) AND attempts>0 AND status<>'done')`, id); err != nil {
			return uuid.Nil, err
		}
		if uncertain {
			return uuid.Nil, ErrTaskRestartUncertain
		}
	}
	child := uuid.New()
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_tasks
 (id,soul_id,user_id,title,description,acceptance_criteria,handler,strategy,delegate_to,config,plan,tools,use_agents,max_iterations,executor_version,status,progress,deadline)
 SELECT $2,soul_id,user_id,title,description,acceptance_criteria,handler,strategy,delegate_to,
 (COALESCE(config,'{}'::jsonb)-'start_at')||jsonb_build_object('restart_of',id::text),plan,tools,use_agents,max_iterations,executor_version,'pending','{}'::jsonb,
 CASE WHEN executor_version=2 AND schedule IS NULL AND cadence IS NULL AND strategy<>'recurring' THEN NULL ELSE clock_timestamp()+$3*interval '1 millisecond' END
 FROM agent_tasks WHERE id=$1`, id, child, budget.Milliseconds()); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_task_restarts(source_task_id,new_task_id) VALUES($1,$2)`, id, child); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(); err != nil {
		return uuid.Nil, err
	}
	return child, nil
}

// Resume only re-enables a recurring schedule. One-shot execution identity must
// never be revived with its old leases, checkpoints or delivery keys.
func (c *TaskController) Resume(ctx context.Context, user, soul, id uuid.UUID) error {
	if user == uuid.Nil || soul == uuid.Nil || id == uuid.Nil {
		return sql.ErrNoRows
	}
	tx, err := c.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if c.schema != "" {
		if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+pq.QuoteIdentifier(c.schema)+`,pg_catalog`); err != nil {
			return err
		}
	}
	var task AgentTask
	if err := tx.GetContext(ctx, &task, `SELECT * FROM agent_tasks WHERE id=$1 AND user_id=$2 AND soul_id=$3 FOR UPDATE`, id, user, soul); err != nil {
		return err
	}
	if task.Schedule == nil && task.Cadence == nil && task.Strategy != StrategyRecurring {
		return ErrTaskNotRecurring
	}
	if task.Status == "canceled" || task.Status == "paused" {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET status='pending' WHERE id=$1`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
