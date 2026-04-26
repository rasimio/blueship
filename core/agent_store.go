package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// AgentTaskStore provides CRUD and polling queries for agent_tasks.
type AgentTaskStore struct {
	db *sqlx.DB
}

// NewAgentTaskStore creates a store backed by the agent_tasks table.
func NewAgentTaskStore(db *sqlx.DB) *AgentTaskStore {
	return &AgentTaskStore{db: db}
}

// PendingTasks returns tasks ready to be picked up by the scheduler.
func (s *AgentTaskStore) PendingTasks(ctx context.Context) ([]AgentTask, error) {
	var tasks []AgentTask
	err := s.db.SelectContext(ctx, &tasks, `
		SELECT * FROM agent_tasks
		WHERE status = 'pending'
		  AND (max_iterations = 0 OR iteration < max_iterations)
		  AND (deadline IS NULL OR deadline > NOW())
		ORDER BY created_at`)
	return tasks, err
}

// SetRunning marks a task as running. Does NOT increment iteration —
// iteration is incremented only on successful completion (Complete/UpdateProgress)
// so that crashes don't waste iterations.
func (s *AgentTaskStore) SetRunning(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'running', last_run_at = NOW()
		WHERE id = $1`, id)
	return err
}

// UpdateProgress saves intermediate state, increments iteration, and sets the task back to pending.
func (s *AgentTaskStore) UpdateProgress(ctx context.Context, id uuid.UUID, progress json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET progress = $2, status = 'pending', iteration = iteration + 1
		WHERE id = $1`, id, progress)
	return err
}

// Complete marks a task as done with a final result and increments iteration.
func (s *AgentTaskStore) Complete(ctx context.Context, id uuid.UUID, result string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'done', result = $2, completed_at = NOW(), iteration = iteration + 1
		WHERE id = $1`, id, result)
	return err
}

// CompleteExhausted force-terminates tasks that exhausted max_iterations
// without reaching done on their own. The terminal status depends on the
// task's lifecycle:
//   - recurring (schedule != NULL) — left alone; recurring jobs reset
//     to iteration=0 each cycle via ResetForNextRun and never hit the
//     cap legitimately.
//   - one-shot (schedule == NULL) — marked 'failed' with an explanatory
//     error_message. Auto-completing as 'done' was the old behaviour and
//     was wrong: it claimed success for tasks that, by definition, never
//     met their acceptance criteria.
func (s *AgentTaskStore) CompleteExhausted(ctx context.Context) {
	s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'failed',
		    completed_at = NOW(),
		    error_message = COALESCE(error_message,
		        'max_iterations reached without satisfying acceptance criteria')
		WHERE status = 'pending'
		  AND schedule IS NULL
		  AND max_iterations > 0
		  AND iteration >= max_iterations`)
}

// SetPending resets a task back to pending (for retry after transient errors).
func (s *AgentTaskStore) SetPending(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks SET status = 'pending' WHERE id = $1`, id)
	return err
}

// Fail marks a task as failed with an error message.
func (s *AgentTaskStore) Fail(ctx context.Context, id uuid.UUID, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'failed', error_message = $2
		WHERE id = $1`, id, errMsg)
	return err
}

// Cancel marks a pending or running task as done with cancellation message.
func (s *AgentTaskStore) Cancel(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'done', result = 'Cancelled by user', completed_at = NOW()
		WHERE id = $1 AND status IN ('pending', 'running')`, id)
	return err
}

// ResetStale resets tasks stuck in 'running' state back to 'pending' (crash recovery).
func (s *AgentTaskStore) ResetStale(ctx context.Context, staleAfter time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'pending'
		WHERE status = 'running' AND last_run_at < $1`,
		time.Now().Add(-staleAfter))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ResetForNextRun resets a completed recurring task back to pending for the next schedule.
func (s *AgentTaskStore) ResetForNextRun(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'pending', iteration = 0, progress = '{}',
		    result = NULL, error_message = NULL, completed_at = NULL
		WHERE id = $1`, id)
	return err
}

// Create inserts a new task and returns it with the generated ID.
//
// Strategy defaults to "recurring" so existing call sites that only set
// Handler+Schedule keep their semantics. New callers explicitly set
// Strategy to one of {direct, structured, delegate} along with the
// matching fields (Plan / AcceptanceCriteria / DelegateTo / UseAgents).
func (s *AgentTaskStore) Create(ctx context.Context, task AgentTask) (AgentTask, error) {
	if task.ID == uuid.Nil {
		task.ID = uuid.New()
	}
	if task.Config == nil {
		task.Config = json.RawMessage(`{}`)
	}
	if task.Progress == nil {
		task.Progress = json.RawMessage(`{}`)
	}
	if task.Plan == nil {
		task.Plan = json.RawMessage(`{}`)
	}
	if task.Status == "" {
		task.Status = "pending"
	}
	if task.MaxIterations == 0 {
		task.MaxIterations = 10
	}
	if task.Strategy == "" {
		task.Strategy = StrategyRecurring
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_tasks (id, user_id, title, description, handler, config, tools,
		                         schedule, deadline, status, progress, max_iterations,
		                         strategy, delegate_to, plan, use_agents,
		                         acceptance_criteria, session_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		task.ID, task.UserID, task.Title, task.Description,
		task.Handler, task.Config, task.Tools,
		task.Schedule, task.Deadline,
		task.Status, task.Progress, task.MaxIterations,
		task.Strategy, task.DelegateTo, task.Plan, task.UseAgents,
		task.AcceptanceCriteria, task.SessionID)
	if err != nil {
		return AgentTask{}, fmt.Errorf("create agent task: %w", err)
	}
	return s.Get(ctx, task.ID)
}

// Get fetches a task by ID.
func (s *AgentTaskStore) Get(ctx context.Context, id uuid.UUID) (AgentTask, error) {
	var task AgentTask
	err := s.db.GetContext(ctx, &task, `SELECT * FROM agent_tasks WHERE id = $1`, id)
	return task, err
}

// EnsureRecurring creates a recurring task if one doesn't exist for (user_id, handler).
// If one exists, updates the schedule. Uses the unique partial index from migration 014.
func (s *AgentTaskStore) EnsureRecurring(ctx context.Context, userID uuid.UUID, handler, schedule, title string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_tasks (user_id, handler, schedule, title, status, max_iterations,
		                         config, progress, strategy, plan)
		VALUES ($1, $2, $3, $4, 'pending', 1, '{}', '{}', 'recurring', '{}')
		ON CONFLICT (user_id, handler) WHERE schedule IS NOT NULL AND status != 'failed'
		DO UPDATE SET schedule = EXCLUDED.schedule, title = EXCLUDED.title`,
		userID, handler, schedule, title)
	return err
}

// Approve resumes a paused task (used after manual review milestones).
func (s *AgentTaskStore) Approve(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'pending'
		WHERE id = $1 AND status = 'paused'`, id)
	return err
}

// PauseTask saves progress, increments iteration, and sets status to 'paused'.
// Used by handlers that need to wait for an external event (e.g. A2A callback).
func (s *AgentTaskStore) PauseTask(ctx context.Context, id uuid.UUID, progress json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET progress = $2, status = 'paused', iteration = iteration + 1
		WHERE id = $1`, id, progress)
	return err
}

// WakePausedByPeerTask finds a paused task waiting for the given peer task ID
// and sets it back to pending. Returns the task ID or sql.ErrNoRows if none found.
func (s *AgentTaskStore) WakePausedByPeerTask(ctx context.Context, peerTaskID string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.GetContext(ctx, &id, `
		UPDATE agent_tasks
		SET status = 'pending'
		WHERE status = 'paused'
		  AND progress->>'peer_task_id' = $1
		RETURNING id`, peerTaskID)
	return id, err
}

// WakeStalePaused resets paused tasks back to pending if they've been paused too long
// (safety net for lost callbacks). Returns the number of tasks woken.
func (s *AgentTaskStore) WakeStalePaused(ctx context.Context, staleAfter time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'pending'
		WHERE status = 'paused' AND last_run_at < $1`,
		time.Now().Add(-staleAfter))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListForUser returns tasks for a user, optionally filtered by status.
func (s *AgentTaskStore) ListForUser(ctx context.Context, userID uuid.UUID, status string) ([]AgentTask, error) {
	var tasks []AgentTask
	if status != "" {
		err := s.db.SelectContext(ctx, &tasks,
			`SELECT * FROM agent_tasks WHERE user_id = $1 AND status = $2 ORDER BY created_at DESC`, userID, status)
		return tasks, err
	}
	err := s.db.SelectContext(ctx, &tasks,
		`SELECT * FROM agent_tasks WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	return tasks, err
}
