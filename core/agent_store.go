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

// SetRunning atomically marks a task as running and increments the iteration.
func (s *AgentTaskStore) SetRunning(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'running', last_run_at = NOW(), iteration = iteration + 1
		WHERE id = $1`, id)
	return err
}

// UpdateProgress saves intermediate state and sets the task back to pending.
func (s *AgentTaskStore) UpdateProgress(ctx context.Context, id uuid.UUID, progress json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET progress = $2, status = 'pending'
		WHERE id = $1`, id, progress)
	return err
}

// Complete marks a task as done with a final result.
func (s *AgentTaskStore) Complete(ctx context.Context, id uuid.UUID, result string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'done', result = $2, completed_at = NOW()
		WHERE id = $1`, id, result)
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
	if task.Status == "" {
		task.Status = "pending"
	}
	if task.MaxIterations == 0 {
		task.MaxIterations = 10
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_tasks (id, user_id, title, description, handler, config, tools,
		                         schedule, deadline, status, progress, max_iterations)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		task.ID, task.UserID, task.Title, task.Description,
		task.Handler, task.Config, task.Tools,
		task.Schedule, task.Deadline,
		task.Status, task.Progress, task.MaxIterations)
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
