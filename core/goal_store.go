package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// GoalStore provides CRUD + lifecycle operations for the goals table.
// Mirrors the shape of AgentTaskStore intentionally so handler code that
// migrates between the two types has one mental model.
type GoalStore struct {
	db *sqlx.DB
}

// NewGoalStore creates a store backed by the goals table.
func NewGoalStore(db *sqlx.DB) *GoalStore {
	return &GoalStore{db: db}
}

// ----- polling / scheduling --------------------------------------------

// PendingGoals returns goals ready to be picked up by the scheduler.
func (s *GoalStore) PendingGoals(ctx context.Context) ([]Goal, error) {
	var goals []Goal
	err := s.db.SelectContext(ctx, &goals, `
		SELECT * FROM goals
		WHERE status = 'pending'
		  AND (max_iterations = 0 OR iteration < max_iterations)
		ORDER BY created_at`)
	return goals, err
}

// SetRunning marks a goal as running (without incrementing iteration —
// iteration is incremented only on successful Complete/UpdateProgress so
// crashes don't consume iteration budget).
func (s *GoalStore) SetRunning(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE goals SET status = 'running', last_run_at = NOW() WHERE id = $1`, id)
	return err
}

// UpdateProgress saves intermediate state, increments iteration, and
// returns the goal to pending so the scheduler picks it up next tick.
func (s *GoalStore) UpdateProgress(ctx context.Context, id uuid.UUID, progress json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE goals
		SET progress = $2, status = 'pending', iteration = iteration + 1
		WHERE id = $1`, id, progress)
	return err
}

// PauseGoal saves progress and transitions to paused. Used when handlers
// are waiting for an external event (e.g. A2A callback from a peer).
func (s *GoalStore) PauseGoal(ctx context.Context, id uuid.UUID, progress json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE goals
		SET progress = $2, status = 'paused', iteration = iteration + 1
		WHERE id = $1`, id, progress)
	return err
}

// WakePausedByPeerTask finds a paused goal waiting for the given peer task
// and transitions it back to pending. Mirror of AgentTaskStore.WakePausedByPeerTask
// for goal-driven peer delegation.
func (s *GoalStore) WakePausedByPeerTask(ctx context.Context, peerTaskID string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.GetContext(ctx, &id, `
		UPDATE goals
		SET status = 'pending'
		WHERE status = 'paused'
		  AND progress->>'peer_task_id' = $1
		RETURNING id`, peerTaskID)
	return id, err
}

// WakeStalePaused resets paused goals back to pending if they've been paused
// longer than staleAfter (safety net for lost callbacks).
func (s *GoalStore) WakeStalePaused(ctx context.Context, staleAfter time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE goals
		SET status = 'pending'
		WHERE status = 'paused' AND last_run_at < $1`,
		time.Now().Add(-staleAfter))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ResetStaleRunning resets goals stuck in running state back to pending
// (crash recovery).
func (s *GoalStore) ResetStaleRunning(ctx context.Context, staleAfter time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE goals
		SET status = 'pending'
		WHERE status = 'running' AND last_run_at < $1`,
		time.Now().Add(-staleAfter))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ----- terminal transitions --------------------------------------------

// Complete marks the goal done with a result.
func (s *GoalStore) Complete(ctx context.Context, id uuid.UUID, result string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE goals
		SET status = 'done', result = $2, completed_at = NOW(), iteration = iteration + 1
		WHERE id = $1`, id, result)
	return err
}

// Fail marks the goal failed with an error message.
func (s *GoalStore) Fail(ctx context.Context, id uuid.UUID, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE goals
		SET status = 'failed', error_message = $2, completed_at = NOW()
		WHERE id = $1`, id, errMsg)
	return err
}

// Cancel transitions an active goal (pending/running/paused) to canceled.
func (s *GoalStore) Cancel(ctx context.Context, id uuid.UUID, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE goals
		SET status = 'canceled', result = $2, completed_at = NOW()
		WHERE id = $1 AND status IN ('pending','running','paused')`, id, reason)
	return err
}

// CompleteExhausted marks goals done if they've burned through max_iterations.
func (s *GoalStore) CompleteExhausted(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE goals
		SET status = 'failed',
		    error_message = COALESCE(error_message, '') || ' [iteration budget exhausted]',
		    completed_at = NOW()
		WHERE status IN ('pending','running') AND max_iterations > 0 AND iteration >= max_iterations`)
	return err
}

// ----- lookups ---------------------------------------------------------

// Get fetches a goal by ID.
func (s *GoalStore) Get(ctx context.Context, id uuid.UUID) (Goal, error) {
	var g Goal
	err := s.db.GetContext(ctx, &g, `SELECT * FROM goals WHERE id = $1`, id)
	return g, err
}

// ListForUser returns goals for a user, optionally filtered by status.
func (s *GoalStore) ListForUser(ctx context.Context, userID uuid.UUID, status string) ([]Goal, error) {
	var goals []Goal
	if status != "" {
		err := s.db.SelectContext(ctx, &goals,
			`SELECT * FROM goals WHERE user_id = $1 AND status = $2 ORDER BY created_at DESC`, userID, status)
		return goals, err
	}
	err := s.db.SelectContext(ctx, &goals,
		`SELECT * FROM goals WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	return goals, err
}

// ----- creation --------------------------------------------------------

// Create inserts a new goal. Returns it hydrated from the DB (so defaults
// applied by the table definition are visible).
func (s *GoalStore) Create(ctx context.Context, g Goal) (Goal, error) {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	if g.Strategy == "" {
		g.Strategy = GoalStrategyStructured
	}
	if g.Config == nil {
		g.Config = json.RawMessage(`{}`)
	}
	if g.Progress == nil {
		g.Progress = json.RawMessage(`{}`)
	}
	if g.Status == "" {
		g.Status = GoalStatusPending
	}
	if g.MaxIterations == 0 {
		g.MaxIterations = 20
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO goals (
			id, user_id, title, description,
			strategy, delegate_to, config, tools,
			status, progress, max_iterations, session_id
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		g.ID, g.UserID, g.Title, g.Description,
		string(g.Strategy), g.DelegateTo, g.Config, pq.StringArray(g.Tools),
		string(g.Status), g.Progress, g.MaxIterations, g.SessionID)
	if err != nil {
		return Goal{}, fmt.Errorf("insert goal: %w", err)
	}
	return s.Get(ctx, g.ID)
}
