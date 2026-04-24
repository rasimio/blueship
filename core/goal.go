package core

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// GoalStrategy controls how the runtime executes a Goal.
type GoalStrategy string

const (
	// GoalStrategyDirect runs a single LLM loop with the full tool registry.
	// No pre-planned steps. The agent decides what to do on each iteration.
	// Good fit for open-ended research / conversational problem-solving.
	GoalStrategyDirect GoalStrategy = "direct"

	// GoalStrategyStructured asks the LLM to produce a JSON plan up front,
	// then the executor interprets steps mechanically (tool calls, waits,
	// decide points) — consulting the LLM only at explicit decision gates.
	// Good fit for pipelines with known shape (e.g. code delivery).
	GoalStrategyStructured GoalStrategy = "structured"

	// GoalStrategyDelegate hands ownership to a peer agent's Ship. The peer
	// runs the full lifecycle on their own runtime and reports milestones
	// back. This Ship just tracks state and surfaces milestones to the owner.
	GoalStrategyDelegate GoalStrategy = "delegate"
)

// GoalStatus is the lifecycle state of a Goal.
type GoalStatus string

const (
	GoalStatusPending  GoalStatus = "pending"
	GoalStatusRunning  GoalStatus = "running"
	GoalStatusPaused   GoalStatus = "paused"
	GoalStatusDone     GoalStatus = "done"
	GoalStatusFailed   GoalStatus = "failed"
	GoalStatusCanceled GoalStatus = "canceled"
)

// Goal is a long-running autonomous task. Unlike AgentTask (which also
// serves recurring scheduled jobs), Goal is always a one-off objective
// with a clear finish line — it completes or fails, it does not cycle.
type Goal struct {
	ID          uuid.UUID `db:"id" json:"id"`
	UserID      uuid.UUID `db:"user_id" json:"user_id"`
	Title       string    `db:"title" json:"title"`
	Description *string   `db:"description" json:"description,omitempty"`

	Strategy   GoalStrategy `db:"strategy" json:"strategy"`
	DelegateTo *string      `db:"delegate_to" json:"delegate_to,omitempty"`

	Config json.RawMessage `db:"config" json:"config"`
	Tools  pq.StringArray  `db:"tools" json:"tools,omitempty"`

	Status       GoalStatus      `db:"status" json:"status"`
	Progress     json.RawMessage `db:"progress" json:"progress"`
	Result       *string         `db:"result" json:"result,omitempty"`
	ErrorMessage *string         `db:"error_message" json:"error_message,omitempty"`

	Iteration     int `db:"iteration" json:"iteration"`
	MaxIterations int `db:"max_iterations" json:"max_iterations"`

	LastRunAt   *time.Time `db:"last_run_at" json:"last_run_at,omitempty"`
	CompletedAt *time.Time `db:"completed_at" json:"completed_at,omitempty"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`

	SessionID *string `db:"session_id" json:"session_id,omitempty"`
}

// IsTerminal reports whether the goal has reached a final state.
func (g *Goal) IsTerminal() bool {
	switch g.Status {
	case GoalStatusDone, GoalStatusFailed, GoalStatusCanceled:
		return true
	}
	return false
}

// GoalHandler executes one iteration of a goal. Implementations correspond
// to GoalStrategy values (direct / structured / delegate). Handlers are
// stateless — all persistent state lives in Goal.Progress.
type GoalHandler interface {
	Run(ctx context.Context, goal Goal, deps AgentDeps) (IterationResult, error)
}
