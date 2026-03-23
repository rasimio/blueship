package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// AgentHandler executes one iteration of an autonomous task.
// Handlers are stateless — all state lives in AgentTask.Progress.
type AgentHandler interface {
	// Run executes one iteration. Returns done=true when the task is complete.
	Run(ctx context.Context, task AgentTask, deps AgentDeps) (IterationResult, error)
	// DefaultTools returns tool names this handler needs. nil = all tools.
	DefaultTools() []string
}

// AgentTask is a persistent autonomous task (maps to agent_tasks table).
type AgentTask struct {
	ID          uuid.UUID       `db:"id" json:"id"`
	UserID      uuid.UUID       `db:"user_id" json:"user_id"`
	Title       string          `db:"title" json:"title"`
	Description *string         `db:"description" json:"description,omitempty"`

	Handler       string          `db:"handler" json:"handler"`
	Config        json.RawMessage `db:"config" json:"config"`
	Tools         pq.StringArray  `db:"tools" json:"tools,omitempty"`

	Schedule *string    `db:"schedule" json:"schedule,omitempty"`
	Deadline *time.Time `db:"deadline" json:"deadline,omitempty"`

	Status       string          `db:"status" json:"status"`
	Progress     json.RawMessage `db:"progress" json:"progress"`
	Result       *string         `db:"result" json:"result,omitempty"`
	ErrorMessage *string         `db:"error_message" json:"error_message,omitempty"`

	Iteration     int        `db:"iteration" json:"iteration"`
	MaxIterations int        `db:"max_iterations" json:"max_iterations"`
	LastRunAt     *time.Time `db:"last_run_at" json:"last_run_at,omitempty"`
	CompletedAt   *time.Time `db:"completed_at" json:"completed_at,omitempty"`
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
}

// IterationResult is returned by AgentHandler.Run after each iteration.
type IterationResult struct {
	Done     bool            // true = task complete
	Progress json.RawMessage // saved to DB between iterations
	Output   string          // final text (when Done=true)
	Notify   string          // send to user immediately (milestone, blocker)
}

// AgentDeps is a focused dependency bundle for agent handlers.
type AgentDeps struct {
	LLM       CompletionProvider
	Registry  *ToolRegistry
	RoleTools *RoleToolStore
	Store     MessageStore // session/message persistence for agent loops
	Prompts   PromptStore
	Logger    *slog.Logger
	DB        func(module string) (*sqlx.DB, error)
	UserID    uuid.UUID
	Config    *Config
}

// SessionManager creates and manages chat sessions for agent handlers.
// Defined as interface to avoid import cycle with session package.
type SessionManager interface {
	GetOrCreate(ctx context.Context, userID, model string) (sessionID string, compactSummary string, err error)
	Create(ctx context.Context, userID, model string) (sessionID string, err error)
}
