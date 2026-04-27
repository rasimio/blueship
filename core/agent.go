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
//
// One primitive covers four execution shapes, distinguished by Strategy:
//   - "recurring"  — Handler is invoked on every Schedule tick. Used for
//                     heartbeat / inner-thought / session-summary jobs.
//   - "direct"    — single LLM cycle with the configured tools. Cortex
//                     freely chooses what to do; finishes when the
//                     AcceptanceCriteria evaluator says it's done.
//   - "structured"— Plan is an ordered list of steps the executor walks
//                     through; each iteration may revise on REVISE; ends
//                     when AcceptanceCriteria is met.
//   - "delegate"  — Plan is shipped to DelegateTo (a peer agent_id) via
//                     A2A. The peer runs the lifecycle locally and
//                     emits milestone events.
//
// Completion is criteria-driven: handlers / executors no longer rely on
// Iteration >= MaxIterations to mark a task done. MaxIterations remains
// only as a runaway-safety cap.
type AgentTask struct {
	ID          uuid.UUID       `db:"id" json:"id"`
	UserID      uuid.UUID       `db:"user_id" json:"user_id"`
	Title       string          `db:"title" json:"title"`
	Description *string         `db:"description" json:"description,omitempty"`

	// AcceptanceCriteria is plain-language describing what "done" means.
	// Each iteration's output is checked against this; an explicit Done
	// signal from the evaluator (or the handler) ends the task.
	AcceptanceCriteria *string `db:"acceptance_criteria" json:"acceptance_criteria,omitempty"`

	Strategy   string  `db:"strategy" json:"strategy"`
	Handler    string  `db:"handler" json:"handler,omitempty"` // empty for non-recurring strategies
	DelegateTo *string `db:"delegate_to" json:"delegate_to,omitempty"`

	Config json.RawMessage `db:"config" json:"config"`
	Plan   json.RawMessage `db:"plan" json:"plan"`

	Tools     pq.StringArray `db:"tools" json:"tools,omitempty"`
	UseAgents pq.StringArray `db:"use_agents" json:"use_agents,omitempty"`

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

	SessionID *string `db:"session_id" json:"session_id,omitempty"`
}

// Strategy values.
const (
	StrategyRecurring  = "recurring"
	StrategyDirect     = "direct"
	StrategyStructured = "structured"
	StrategyDelegate   = "delegate"
)

// IterationResult is returned by AgentHandler.Run after each iteration.
type IterationResult struct {
	Done     bool            // true = task complete
	Pause    bool            // true = pause until external wakeup (e.g. A2A callback)
	Progress json.RawMessage // saved to DB between iterations
	Output   string          // final text (when Done=true)
	Notify   string          // send to user immediately (milestone, blocker)
}

// TaskProgress is structured progress for multi-iteration background tasks.
// Handlers marshal this into IterationResult.Progress between iterations.
type TaskProgress struct {
	Phase     string   `json:"phase"`      // "researching", "synthesizing", "complete"
	Findings  []string `json:"findings"`   // accumulated results
	NextSteps []string `json:"next_steps"` // plan for next iteration
	Summary   string   `json:"summary"`    // running summary for status checks
}

// AgentDeps is a focused dependency bundle for agent handlers.
type AgentDeps struct {
	LLM        CompletionProvider
	Embedder   EmbeddingProvider    // nil = embedding disabled
	Registry   *ToolRegistry
	RoleTools  *RoleToolStore
	ModelStore *ModelConfigStore // model role → provider:model (nil = use Config.Models)
	Store      MessageStore     // session/message persistence for agent loops
	Prompts    PromptStore
	Users      UserStore        // nil = user lookup disabled
	Sessions   SessionQuerier   // nil = session query disabled
	Logger     *slog.Logger
	DB         func(module string) (*sqlx.DB, error)
	UserID     uuid.UUID
	Config     *Config

	// SelfAgentID is the Ship's own Fleet-issued agent id. Empty until
	// the first Fleet bootstrap call completes; used by delegate-strategy
	// handlers so the peer can route status callbacks back here.
	SelfAgentID func() string

	// ContextInjector builds per-request context (active notes, etc.) for the agent loop.
	ContextInjector func(ctx context.Context, userID, message string) string

	// ReflexPreparer returns structured context for the reflex pipeline.
	ReflexPreparer func(ctx context.Context, userID, message string) *ReflexContext

	// RuleEngine evaluates structured rules against context.
	// Returns matched rules with guidance, pre-actions, and tool restrictions.
	RuleEngine func(ctx context.Context, rc RuleContext) []ActiveRule
}

// SessionManager creates and manages chat sessions for agent handlers.
// Defined as interface to avoid import cycle with session package.
type SessionManager interface {
	GetOrCreate(ctx context.Context, userID, model string) (sessionID string, compactSummary string, err error)
	Create(ctx context.Context, userID, model string) (sessionID string, err error)
}
