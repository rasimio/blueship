package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type taskRunKey struct{}

// Each task can fan out into three read workers. Bound owner admission so a
// queue burst from one person cannot occupy the whole global task pool.
const MaxConcurrentGraphTasksPerUser = 2

// Match the scheduler's ISO start_at gate before ranking candidates, so future
// work cannot hide ready tasks behind an owner's admission limit. Malformed
// values retain the legacy behavior of an absent gate.
const graphStartAtSQL = `(CASE WHEN t.config->>'start_at' ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.,][0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$'
 AND pg_input_is_valid(t.config->>'start_at','timestamp with time zone') THEN (t.config->>'start_at')::timestamptz END)`

func WithTaskRunID(ctx context.Context, run uuid.UUID) context.Context {
	return context.WithValue(ctx, taskRunKey{}, run)
}

// requireTaskRun runs while the task row is locked. Every v2 worker mutation
// must carry the current coordinator ID, not merely a still-live step ID.
func requireTaskRun(ctx context.Context, tx *sqlx.Tx, id uuid.UUID) error {
	run, _ := ctx.Value(taskRunKey{}).(uuid.UUID)
	if run == uuid.Nil {
		return ErrTaskClaimLost
	}
	var current uuid.UUID
	err := tx.GetContext(ctx, &current, `SELECT run_id FROM agent_task_runs WHERE task_id=$1 AND run_id=$2 AND lease_until>clock_timestamp()`, id, run)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTaskClaimLost
	}
	return err
}

func lockTaskExecution(ctx context.Context, tx *sqlx.Tx, id uuid.UUID, claim *time.Time, deadline bool) error {
	var task AgentTask
	err := tx.GetContext(ctx, &task, `SELECT * FROM agent_tasks WHERE id=$1 FOR UPDATE`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTaskClaimLost
	}
	if err != nil {
		return err
	}
	if task.Status != "running" || (claim != nil && (task.LastRunAt == nil || !task.LastRunAt.Equal(*claim))) {
		return ErrTaskClaimLost
	}
	if deadline && task.Deadline != nil {
		var live bool
		if err := tx.GetContext(ctx, &live, `SELECT $1::timestamptz>clock_timestamp()`, *task.Deadline); err != nil {
			return err
		}
		if !live {
			return ErrTaskClaimLost
		}
	}
	if task.ExecutorVersion == 2 {
		return requireTaskRun(ctx, tx, id)
	}
	return nil
}

type TaskRun struct {
	TaskID        uuid.UUID `db:"task_id"`
	RunID         uuid.UUID `db:"run_id"`
	LeaseUntil    time.Time `db:"lease_until"`
	HeartbeatAt   time.Time `db:"heartbeat_at"`
	NextAttemptAt time.Time `db:"next_attempt_at"`
	StartedAt     time.Time `db:"started_at"`
}

// ClaimGraphTask atomically replaces an expired coordinator and fences its
// unfinished steps. An ambiguous effect is never restarted as a read.
func (s *AgentTaskStore) ClaimGraphTask(ctx context.Context, id uuid.UUID, lease time.Duration) (*TaskRun, error) {
	if lease < time.Second || lease > 10*time.Minute {
		return nil, fmt.Errorf("invalid task lease")
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var task AgentTask
	err = tx.GetContext(ctx, &task, `SELECT t.* FROM agent_tasks t WHERE id=$1 AND executor_version=2 AND status IN ('pending','running') AND (deadline IS NULL OR deadline>clock_timestamp()) AND COALESCE(`+graphStartAtSQL+`<=clock_timestamp(),true) FOR UPDATE`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Serialize admissions for this owner across daemon processes. Counting
	// live leases without this lock permits simultaneous claims to exceed cap.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('graph-owner:' || $1::text,0))`, task.UserID.String()); err != nil {
		return nil, err
	}
	var active int
	if err := tx.GetContext(ctx, &active, `SELECT count(*) FROM agent_task_runs r JOIN agent_tasks t ON t.id=r.task_id
 WHERE t.user_id=$1 AND t.id<>$2 AND t.executor_version=2 AND t.status IN ('pending','running') AND r.lease_until>clock_timestamp()`, task.UserID, id); err != nil {
		return nil, err
	}
	if active >= MaxConcurrentGraphTasksPerUser {
		return nil, nil
	}
	var run TaskRun
	err = tx.GetContext(ctx, &run, `INSERT INTO agent_task_runs(task_id,run_id,lease_until,heartbeat_at) VALUES($1,$2,clock_timestamp()+$3*interval '1 millisecond',clock_timestamp())
 ON CONFLICT(task_id) DO UPDATE SET run_id=EXCLUDED.run_id,lease_until=EXCLUDED.lease_until,heartbeat_at=EXCLUDED.heartbeat_at,started_at=clock_timestamp()
 WHERE agent_task_runs.lease_until<=clock_timestamp() AND agent_task_runs.next_attempt_at<=clock_timestamp() RETURNING *`, id, uuid.New(), lease.Milliseconds())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET status='running',last_run_at=clock_timestamp(),progress=(COALESCE(progress,'{}'::jsonb)-'next_attempt_at')||'{"phase":"running"}'::jsonb WHERE id=$1`, id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_task_steps SET status=CASE WHEN `+stepNeedsReconciliationSQL+` THEN 'reconciliation' WHEN attempts>=max_attempts THEN 'blocked' ELSE 'pending' END,lease_until=NULL,last_error='coordinator lease lost' WHERE task_id=$1 AND status='running'`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *AgentTaskStore) RenewGraphTask(ctx context.Context, id uuid.UUID, lease time.Duration) error {
	if lease < time.Second || lease > 10*time.Minute {
		return fmt.Errorf("invalid task lease")
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockTaskExecution(ctx, tx, id, nil, true); err != nil {
		return err
	}
	if err := requireTaskRun(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_task_runs SET lease_until=clock_timestamp()+$2*interval '1 millisecond',heartbeat_at=clock_timestamp() WHERE task_id=$1`, id, lease.Milliseconds()); err != nil {
		return err
	}
	return tx.Commit()
}

// YieldGraphTask releases the coordinator during backoff; no compute slot or
// live lease is retained. Active step workers must be drained first.
func (s *AgentTaskStore) YieldGraphTask(ctx context.Context, id uuid.UUID, next time.Time) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockTaskExecution(ctx, tx, id, nil, false); err != nil {
		return err
	}
	if err := requireTaskRun(ctx, tx, id); err != nil {
		return err
	}
	var active bool
	if err := tx.GetContext(ctx, &active, `SELECT EXISTS(SELECT 1 FROM agent_task_steps WHERE task_id=$1 AND status='running')`, id); err != nil {
		return err
	}
	if active {
		return fmt.Errorf("cannot yield graph with running steps")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_task_runs SET lease_until=clock_timestamp(),next_attempt_at=$2 WHERE task_id=$1`, id, next); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET status='pending',progress=COALESCE(progress,'{}'::jsonb)||jsonb_build_object('phase','retry_wait','next_attempt_at',$2::timestamptz) WHERE id=$1`, id, next); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AgentTaskStore) RunnableGraphTasks(ctx context.Context, limit int) ([]AgentTask, error) {
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid dispatch limit")
	}
	var tasks []AgentTask
	err := s.db.SelectContext(ctx, &tasks, `WITH eligible AS (
 SELECT t.id,t.last_run_at,t.created_at,
 row_number() OVER (PARTITION BY t.user_id ORDER BY t.last_run_at NULLS FIRST,t.created_at,t.id) AS owner_rank,
 (SELECT count(*) FROM agent_task_runs live JOIN agent_tasks owned ON owned.id=live.task_id
  WHERE owned.user_id=t.user_id AND owned.executor_version=2 AND owned.status IN ('pending','running') AND live.lease_until>clock_timestamp()) AS active
 FROM agent_tasks t LEFT JOIN agent_task_runs r ON r.task_id=t.id
 WHERE t.executor_version=2 AND t.status IN ('pending','running') AND (t.deadline IS NULL OR t.deadline>clock_timestamp())
 AND (r.task_id IS NULL OR (r.lease_until<=clock_timestamp() AND r.next_attempt_at<=clock_timestamp()))
 AND COALESCE(`+graphStartAtSQL+`<=clock_timestamp(),true)
 ) SELECT t.* FROM eligible e JOIN agent_tasks t ON t.id=e.id
 WHERE e.owner_rank+e.active<=$2
 ORDER BY e.owner_rank+e.active,e.last_run_at NULLS FIRST,e.created_at,e.id LIMIT $1`, limit, MaxConcurrentGraphTasksPerUser)
	return tasks, err
}

func (s *AgentTaskStore) SaveGraphProgress(ctx context.Context, id uuid.UUID, progress json.RawMessage) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockTaskExecution(ctx, tx, id, nil, true); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET progress=COALESCE(progress,'{}'::jsonb)||$2::jsonb WHERE id=$1`, id, jsonbObject(progress)); err != nil {
		return err
	}
	return tx.Commit()
}

// ExpiredGraphTasks is independent of admission and worker ownership: queued,
// paused and running v2 tasks all terminate through the artifact finalizer.
func (s *AgentTaskStore) ExpiredGraphTasks(ctx context.Context, now time.Time, limit int) ([]AgentTask, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("invalid expiry limit")
	}
	var tasks []AgentTask
	err := s.db.SelectContext(ctx, &tasks, `SELECT * FROM agent_tasks WHERE executor_version=2 AND status IN ('pending','running','paused') AND deadline<=$1 ORDER BY deadline,id LIMIT $2`, now, limit)
	// One-shot graph tasks run to completion even if an older host stored a
	// deadline for them.
	bounded := tasks[:0]
	for _, task := range tasks {
		if TaskStopDeadline(task) != nil {
			bounded = append(bounded, task)
		}
	}
	return bounded, err
}

// NextGraphWake returns the next persisted retry/lease or deadline boundary.
// Already runnable rows are excluded: capacity-release hints wake that queue.
func (s *AgentTaskStore) NextGraphWake(ctx context.Context) (time.Time, error) {
	var next sql.NullTime
	err := s.db.GetContext(ctx, &next, `SELECT min(boundary.at) FROM agent_tasks t
 LEFT JOIN agent_task_runs r ON r.task_id=t.id
 CROSS JOIN LATERAL (VALUES(t.deadline),(CASE WHEN t.status IN ('pending','running') THEN GREATEST(r.lease_until,r.next_attempt_at,`+graphStartAtSQL+`) END)) boundary(at)
 WHERE t.executor_version=2 AND t.status IN ('pending','running','paused') AND boundary.at>clock_timestamp()`)
	if err != nil {
		return time.Time{}, err
	}
	return next.Time, nil
}
