package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// TaskStep is a concrete deliverable with explicit dependencies and acceptance.
// Kind action is never automatically replayed after an ambiguous worker loss.
type TaskStep struct {
	ProgressAt    *time.Time      `db:"progress_at" json:"progress_at,omitempty"`
	Tools         pq.StringArray  `db:"tools" json:"tools"`
	NextAttemptAt time.Time       `db:"next_attempt_at" json:"next_attempt_at"`
	LastError     string          `db:"last_error" json:"last_error,omitempty"`
	TaskID        uuid.UUID       `db:"task_id" json:"task_id"`
	ID            string          `db:"step_id" json:"id"`
	Goal          string          `db:"goal" json:"goal"`
	Acceptance    string          `db:"acceptance" json:"acceptance"`
	Kind          string          `db:"kind" json:"kind"`
	Dependencies  pq.StringArray  `db:"dependencies" json:"dependencies"`
	Status        string          `db:"status" json:"status"`
	Result        string          `db:"result" json:"result"`
	Checkpoint    json.RawMessage `db:"checkpoint" json:"checkpoint"`
	Attempts      int             `db:"attempts" json:"attempts"`
	MaxAttempts   int             `db:"max_attempts" json:"max_attempts"`
	RunID         *uuid.UUID      `db:"run_id" json:"run_id,omitempty"`
	LeaseUntil    *time.Time      `db:"lease_until" json:"lease_until,omitempty"`
	HeartbeatAt   *time.Time      `db:"heartbeat_at" json:"heartbeat_at,omitempty"`
	StartedAt     *time.Time      `db:"started_at" json:"started_at,omitempty"`
	CompletedAt   *time.Time      `db:"completed_at" json:"completed_at,omitempty"`
	EstimatedMS   *int64          `db:"estimated_ms" json:"estimated_ms,omitempty"`
}

func ValidateTaskSteps(steps []TaskStep) error {
	if len(steps) == 0 || len(steps) > 64 {
		return fmt.Errorf("plan must contain 1 to 64 steps")
	}
	byID := map[string]TaskStep{}
	for _, step := range steps {
		if step.ID == "" || len(step.ID) > 80 || strings.TrimSpace(step.Goal) == "" || strings.TrimSpace(step.Acceptance) == "" {
			return fmt.Errorf("step requires id, goal and acceptance")
		}
		if _, ok := byID[step.ID]; ok {
			return fmt.Errorf("duplicate step %q", step.ID)
		}
		if step.Kind != "read" && step.Kind != "action" && step.Kind != "finalize" {
			return fmt.Errorf("invalid step kind %q", step.Kind)
		}
		if step.MaxAttempts < 0 || step.MaxAttempts > 10 {
			return fmt.Errorf("invalid attempt budget")
		}
		if step.EstimatedMS != nil && *step.EstimatedMS <= 0 {
			return fmt.Errorf("invalid time estimate")
		}
		byID[step.ID] = step
	}
	visited, visiting := map[string]bool{}, map[string]bool{}
	var walk func(string) error
	walk = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("dependency cycle at %q", id)
		}
		if visited[id] {
			return nil
		}
		step, ok := byID[id]
		if !ok {
			return fmt.Errorf("unknown dependency %q", id)
		}
		visiting[id] = true
		seen := map[string]bool{}
		for _, dep := range step.Dependencies {
			if seen[dep] {
				return fmt.Errorf("duplicate dependency %q", dep)
			}
			seen[dep] = true
			if err := walk(dep); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range byID {
		if err := walk(id); err != nil {
			return err
		}
	}
	return nil
}

// SaveTaskSteps accepts a plan only from the current task claim. Replanning
// completed steps needs an explicit revision operation, never an overwrite.
func (s *AgentTaskStore) SaveTaskSteps(ctx context.Context, id uuid.UUID, claim time.Time, steps []TaskStep) error {
	if err := ValidateTaskSteps(steps); err != nil {
		return err
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockTaskExecution(ctx, tx, id, &claim, false); err != nil {
		return err
	}
	for _, step := range steps {
		if step.MaxAttempts == 0 {
			step.MaxAttempts = 3
		}
		if step.Tools == nil {
			step.Tools = pq.StringArray{}
		}
		if step.Dependencies == nil {
			step.Dependencies = pq.StringArray{}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO agent_task_steps(task_id,step_id,goal,acceptance,kind,dependencies,max_attempts,estimated_ms,tools) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, step.ID, step.Goal, step.Acceptance, step.Kind, step.Dependencies, step.MaxAttempts, step.EstimatedMS, step.Tools)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *AgentTaskStore) TaskSteps(ctx context.Context, id uuid.UUID) ([]TaskStep, error) {
	var steps []TaskStep
	err := s.db.SelectContext(ctx, &steps, `SELECT * FROM agent_task_steps WHERE task_id=$1 ORDER BY step_id`, id)
	return steps, err
}

// ClaimTaskStep serializes with task cancellation and gates on confirmed
// dependencies. The run ID fences writes even if an old worker is still alive.
func (s *AgentTaskStore) ClaimTaskStep(ctx context.Context, id uuid.UUID, stepID string, lease time.Duration) (*TaskStep, error) {
	if lease < time.Second || lease > 10*time.Minute {
		return nil, fmt.Errorf("invalid step lease")
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := lockTaskExecution(ctx, tx, id, nil, true); err != nil {
		return nil, err
	}
	var step TaskStep
	err = tx.GetContext(ctx, &step, `UPDATE agent_task_steps s SET status='running',run_id=$3,lease_until=clock_timestamp()+$4*interval '1 millisecond',heartbeat_at=clock_timestamp(),started_at=COALESCE(started_at,clock_timestamp()),attempts=attempts+1
 WHERE task_id=$1 AND step_id=$2 AND status='pending' AND next_attempt_at<=clock_timestamp() AND attempts<max_attempts
 AND NOT EXISTS (SELECT 1 FROM agent_task_steps peer WHERE peer.task_id=s.task_id AND peer.status IN ('running','reconciliation') AND (s.kind<>'read' OR peer.kind<>'read'))
 AND NOT EXISTS (SELECT 1 FROM unnest(s.dependencies) dep LEFT JOIN agent_task_steps parent ON parent.task_id=s.task_id AND parent.step_id=dep WHERE parent.status IS DISTINCT FROM 'done') RETURNING s.*`, id, stepID, uuid.New(), lease.Milliseconds())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &step, nil
}

// CheckpointTaskStep renews a live claim and persists its partial result. Once
// expired, a claim cannot revive itself, even before the recovery scan runs.
func (s *AgentTaskStore) CheckpointTaskStep(ctx context.Context, id uuid.UUID, stepID string, run uuid.UUID, lease time.Duration, checkpoint json.RawMessage) error {
	if lease < time.Second || lease > 10*time.Minute {
		return fmt.Errorf("invalid step lease")
	}
	return s.updateTaskStep(ctx, id, stepID, run, false, "", checkpoint, lease)
}

// CompleteTaskStep must only be called after the step's acceptance gate passes.
// Empty results never satisfy a dependency.
func (s *AgentTaskStore) CompleteTaskStep(ctx context.Context, id uuid.UUID, stepID string, run uuid.UUID, result string) error {
	if strings.TrimSpace(result) == "" {
		return fmt.Errorf("completed step requires a result")
	}
	return s.updateTaskStep(ctx, id, stepID, run, true, result, nil, 0)
}

func (s *AgentTaskStore) updateTaskStep(ctx context.Context, id uuid.UUID, stepID string, run uuid.UUID, complete bool, result string, checkpoint json.RawMessage, lease time.Duration) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockTaskExecution(ctx, tx, id, nil, true); err != nil {
		return err
	}
	var res sql.Result
	if complete {
		res, err = tx.ExecContext(ctx, `UPDATE agent_task_steps SET status='done',result=$4,completed_at=clock_timestamp(),progress_at=clock_timestamp(),lease_until=NULL WHERE task_id=$1 AND step_id=$2 AND run_id=$3 AND status='running' AND lease_until>clock_timestamp()`, id, stepID, run, result)
	} else {
		res, err = tx.ExecContext(ctx, `UPDATE agent_task_steps SET progress_at=CASE WHEN $4::jsonb IS NOT NULL AND checkpoint IS DISTINCT FROM $4::jsonb THEN clock_timestamp() ELSE progress_at END,checkpoint=COALESCE($4::jsonb,checkpoint),heartbeat_at=clock_timestamp(),lease_until=clock_timestamp()+$5*interval '1 millisecond' WHERE task_id=$1 AND step_id=$2 AND run_id=$3 AND status='running' AND lease_until>clock_timestamp()`, id, stepID, run, nullableStepCheckpoint(checkpoint), lease.Milliseconds())
	}
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
	return tx.Commit()
}

// stepNeedsReconciliationSQL is shared by failure and lease recovery. A saved
// candidate can resume verification without invoking the tool loop. Missing or
// incomplete checkpoints must never grant permission to replay external effects.
const stepNeedsReconciliationSQL = `kind<>'read'
 AND NOT (kind='finalize' AND cardinality(tools)=0)
 AND NOT COALESCE(checkpoint->>'candidate_ready'='true'
   AND checkpoint->>'phase' IN ('verification','verified')
   AND jsonb_typeof(checkpoint->'output')='string'
   AND btrim(checkpoint->>'output')<>''
   AND (checkpoint->'pending_tool' IS NULL OR checkpoint->'pending_tool'='null'::jsonb),false)`

// RecoverTaskSteps retries pure work or verification of a saved candidate.
// Unresolved external effects remain fenced for reconciliation.
func (s *AgentTaskStore) RecoverTaskSteps(ctx context.Context, id uuid.UUID) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockTaskExecution(ctx, tx, id, nil, false); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_task_steps SET status=CASE WHEN `+stepNeedsReconciliationSQL+` THEN 'reconciliation' WHEN attempts>=max_attempts THEN 'blocked' ELSE 'pending' END,lease_until=NULL WHERE task_id=$1 AND status='running' AND lease_until<=clock_timestamp()`, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func nullableStepCheckpoint(cp json.RawMessage) any {
	if len(cp) == 0 {
		return nil
	}
	return cp
}

// FailTaskStep records the error before releasing the worker. Unknown effects
// remain fenced for reconciliation, while read retries have bounded backoff.
func (s *AgentTaskStore) FailTaskStep(ctx context.Context, id uuid.UUID, stepID string, run uuid.UUID, reason string, retryable bool) error {
	return s.FailTaskStepAfter(ctx, id, stepID, run, reason, retryable, 0)
}

func (s *AgentTaskStore) FailTaskStepAfter(ctx context.Context, id uuid.UUID, stepID string, run uuid.UUID, reason string, retryable bool, delay time.Duration) error {

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockTaskExecution(ctx, tx, id, nil, false); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE agent_task_steps SET last_error=$4,
 status=CASE WHEN `+stepNeedsReconciliationSQL+` THEN 'reconciliation' WHEN $5 AND attempts<max_attempts THEN 'pending' ELSE 'blocked' END,
 next_attempt_at=clock_timestamp()+GREATEST(LEAST(60,5*attempts)*1000,$6)*interval '1 millisecond',lease_until=NULL
 WHERE task_id=$1 AND step_id=$2 AND run_id=$3 AND status='running' AND lease_until>clock_timestamp()`, id, stepID, run, reason, retryable, max(0, delay.Milliseconds()))
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
	return tx.Commit()
}

var ErrTaskVerificationUnavailable = errors.New("task verification unavailable")

// ResolveStuckTaskSteps hands work steps that exhausted their attempts, or
// whose external effect could not be confirmed, to their dependents with the
// best saved output and the reason. Nothing is re-run: an unconfirmed action
// is reported as unconfirmed, and the task still reaches its report.
func (s *AgentTaskStore) ResolveStuckTaskSteps(ctx context.Context, id uuid.UUID) (int, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := lockTaskExecution(ctx, tx, id, nil, false); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE agent_task_steps SET status='done',completed_at=clock_timestamp(),lease_until=NULL,
 result=btrim(COALESCE(NULLIF(btrim(checkpoint->>'output'),''),NULLIF(btrim(checkpoint->>'last_candidate'),''),'')
  || E'\n\n[unverified_step_result]\nThis step could not be completed: ' || COALESCE(NULLIF(btrim(last_error),''),'no confirmed result')
  || E'\nUse only facts its sources support and state this gap explicitly in the deliverable.\n[/unverified_step_result]')
 WHERE task_id=$1 AND status IN ('blocked','reconciliation') AND kind<>'finalize'`, id)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}
