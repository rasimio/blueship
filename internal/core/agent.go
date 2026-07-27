package core

import (
	"context"
	"encoding/json"
	"errors"
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
//     heartbeat / inner-thought / session-summary jobs.
//   - "direct"    — single LLM cycle with the configured tools. Cortex
//     freely chooses what to do; finishes when the
//     AcceptanceCriteria evaluator says it's done.
//   - "structured"— Plan is an ordered list of steps the executor walks
//     through; each iteration may revise on REVISE; ends
//     when AcceptanceCriteria is met.
//   - "delegate"  — Plan is shipped to DelegateTo (a peer agent_id) via
//     A2A. The peer runs the lifecycle locally and
//     emits milestone events.
//
// Completion is criteria-driven: handlers / executors no longer rely on
// Iteration >= MaxIterations to mark a task done. MaxIterations remains
// only as a runaway-safety cap.
type AgentTask struct {
	ID          uuid.UUID `db:"id" json:"id"`
	SoulID      uuid.UUID `db:"soul_id" json:"soul_id"`
	UserID      uuid.UUID `db:"user_id" json:"user_id"`
	Title       string    `db:"title" json:"title"`
	Description *string   `db:"description" json:"description,omitempty"`

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

	// Cadence (Go duration string, e.g. "1h", "30m") gates how often a
	// non-recurring task is allowed to tick. Scheduler skips the task if
	// time.Since(LastRunAt) < Cadence — without burning an iteration —
	// so periodic monitors can live on strategy=direct without a
	// recurring schedule. NULL = no rate limit (default per-tick).
	Cadence *string `db:"cadence" json:"cadence,omitempty"`

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

	// RequiredRecheckURLs are URLs the cortex must re-fetch in the next
	// iteration before submitting another report. Populated by Gate C
	// (claim-level grounding) when it rejects with ungrounded attribution
	// or architectural claims tied to a fetched doc. Enforced by Gate B'
	// at the top of evaluateAcceptance — without an in-iteration re-fetch
	// of every URL in this list, the next submit is rejected outright.
	// Stays empty in shadow mode; activates when Gate C flips to enforce.
	RequiredRecheckURLs pq.StringArray `db:"required_recheck_urls" json:"required_recheck_urls,omitempty"`
}

// Strategy values.
const (
	StrategyRecurring  = "recurring"
	StrategyDirect     = "direct"
	StrategyStructured = "structured"
	StrategyDelegate   = "delegate"
)

// TaskDeliveryRef identifies one stable item emitted by one task-program
// input. InputID is part of the identity so unrelated integrations may reuse
// the same provider-owned key without suppressing each other.
type TaskDeliveryRef struct {
	InputID string
	ItemKey string
}

// TaskDeliveryItemKeyMaxBytes keeps provider-owned keys comfortably below
// PostgreSQL btree tuple limits even when combined with the task/input keys.
// Runtime adapters must reject larger keyed output instead of treating it as
// legacy unkeyed data, which would bypass delivery deduplication.
const TaskDeliveryItemKeyMaxBytes = 512

// TaskDeliveryLedger is the read-side persistence boundary for task-program
// delivery deduplication. Implementations must be safe for concurrent
// scheduler runs. MarkDelivered remains for legacy callers; scheduler-owned
// notifications use TaskNotificationJournal so send admission is persisted
// before the transport is called.
type TaskDeliveryLedger interface {
	LookupDelivered(ctx context.Context, taskID uuid.UUID, refs []TaskDeliveryRef) (map[TaskDeliveryRef]bool, error)
	MarkDelivered(ctx context.Context, taskID uuid.UUID, refs []TaskDeliveryRef) error
}

// TaskNotificationReceipt is the transport identity returned after one
// confirmed notification attempt. String fields keep the framework neutral to
// provider-specific identifier widths and make the value safe to persist as
// JSON without precision loss.
type TaskNotificationReceipt struct {
	Transport string `json:"transport"`
	BotID     string `json:"bot_id,omitempty"`
	ChatID    string `json:"chat_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	// DeliveredAt is captured immediately after the provider ACK. Durable
	// post-send projections use it so eager and repaired chat history have
	// identical chronology.
	DeliveredAt time.Time `json:"delivered_at,omitempty"`
}

// TaskNotificationIntent is one immutable journaled message selected for a
// durable retry. Its occurrence reservations remain owned by ID in every
// journal state; retry workers resend Text and never rerun the task program.
type TaskNotificationIntent struct {
	ID     uuid.UUID         `db:"id" json:"id"`
	TaskID uuid.UUID         `db:"task_id" json:"task_id"`
	UserID uuid.UUID         `db:"user_id" json:"user_id"`
	Text   string            `db:"text" json:"text"`
	Refs   []TaskDeliveryRef `db:"-" json:"refs"`
}

type definitelyNotSentError struct {
	err error
}

func (e *definitelyNotSentError) Error() string { return e.err.Error() }
func (e *definitelyNotSentError) Unwrap() error { return e.err }

// DefinitelyNotSent marks an outbound-notification error as a conclusive
// pre-provider rejection. A durable notification worker may retry only errors
// carrying this marker; ordinary timeout/EOF errors remain ambiguous and are
// tombstoned to preserve at-most-once delivery.
func DefinitelyNotSent(err error) error {
	if err == nil || IsDefinitelyNotSent(err) {
		return err
	}
	return &definitelyNotSentError{err: err}
}

// IsDefinitelyNotSent reports whether err was marked with DefinitelyNotSent.
func IsDefinitelyNotSent(err error) bool {
	var target *definitelyNotSentError
	return errors.As(err, &target)
}

type notificationRetryDelayError struct {
	err   error
	delay time.Duration
}

func (e *notificationRetryDelayError) Error() string { return e.err.Error() }
func (e *notificationRetryDelayError) Unwrap() error { return e.err }

// DefinitelyNotSentAfter marks a conclusive pre-provider rejection and carries
// the provider's requested delay for the durable retry. A non-positive delay is
// preserved; callers may substitute their own safe default.
func DefinitelyNotSentAfter(err error, delay time.Duration) error {
	if err == nil || IsPermanentlyNotSent(err) {
		return err
	}
	return DefinitelyNotSent(&notificationRetryDelayError{err: err, delay: delay})
}

// NotificationRetryDelay returns the retry delay attached by
// DefinitelyNotSentAfter. Permanent rejections intentionally never expose a
// delay, even if they wrap a previously delayed error.
func NotificationRetryDelay(err error) (time.Duration, bool) {
	if IsPermanentlyNotSent(err) {
		return 0, false
	}
	var target *notificationRetryDelayError
	if !errors.As(err, &target) {
		return 0, false
	}
	return target.delay, true
}

type permanentlyNotSentError struct {
	err error
}

func (e *permanentlyNotSentError) Error() string { return e.err.Error() }
func (e *permanentlyNotSentError) Unwrap() error { return e.err }

// PermanentlyNotSent marks a conclusive pre-provider rejection that retrying
// cannot heal without an external state change (for example, a blocked bot or
// a non-Telegram primary channel). It is also DefinitelyNotSent, but carries no
// retry delay; the durable notification journal retains and rejects the
// occurrence instead of rerunning its task program.
func PermanentlyNotSent(err error) error {
	if err == nil || IsPermanentlyNotSent(err) {
		return err
	}
	return &permanentlyNotSentError{err: DefinitelyNotSent(err)}
}

// IsPermanentlyNotSent reports whether err was marked with PermanentlyNotSent.
func IsPermanentlyNotSent(err error) bool {
	var target *permanentlyNotSentError
	return errors.As(err, &target)
}

// TaskNotificationJournal is the durable at-most-once admission boundary for
// keyed task-program notifications. BeginNotificationAttempt must atomically
// reserve every ref and persist the immutable user/text intent. created=false
// means this exact occurrence set was already journaled and the caller MUST
// NOT invoke the transport again. A partial overlap with a differently grouped
// attempt is an error so a later scheduled execution can rebuild from only the
// still-free refs. ConfirmNotificationAttempt atomically marks both
// the reservation and all of its refs delivered. An ambiguous transport
// failure is tombstoned with MarkNotificationUncertain rather than retried.
// Explicitly retryable failures retain the immutable intent and are claimed by
// ClaimRetryableNotification without rerunning integrations or the LLM.
type TaskNotificationJournal interface {
	BeginNotificationAttempt(
		ctx context.Context,
		taskID, userID uuid.UUID,
		text string,
		refs []TaskDeliveryRef,
	) (id uuid.UUID, created bool, err error)
	ConfirmNotificationAttempt(ctx context.Context, id uuid.UUID, receipt TaskNotificationReceipt) error
	MarkNotificationUncertain(ctx context.Context, id uuid.UUID, reason string) error
	DeferNotificationAttempt(ctx context.Context, id uuid.UUID, reason string, retryAt time.Time) error
	RejectNotificationAttempt(ctx context.Context, id uuid.UUID, reason string) error
	ClaimRetryableNotification(ctx context.Context, now time.Time) (*TaskNotificationIntent, error)
}

// IterationResult is returned by AgentHandler.Run after each iteration.
type IterationResult struct {
	Done     bool            // true = task complete
	Pause    bool            // true = pause until external wakeup (e.g. A2A callback)
	Progress json.RawMessage // saved to DB between iterations
	Output   string          // final text (when Done=true)
	Notify   string          // send to user immediately (milestone, blocker)

	// Notified is set by the scheduler after it attempts to send Notify to
	// the user. Hook receivers use this for post-delivery side effects such as
	// debounce bookkeeping.
	Notified bool

	// IsFinal is set by the scheduler (NOT the handler) after the
	// acceptance-criteria gate decides whether a Done-claim is the real
	// terminal state. Recurring tasks: handler's Done is authoritative,
	// IsFinal mirrors Done. Non-recurring with criteria: IsFinal is true
	// only when the criteria evaluator agreed; rejected drafts get
	// Done=true / IsFinal=false so AgentIterationCompletedHook receivers
	// (Saver) can avoid persisting "final" reports the gate hasn't
	// approved yet. Non-recurring without criteria: IsFinal mirrors Done.
	// Pause / continue iterations: IsFinal=false.
	IsFinal bool

	// Partial marks a salvaged best-effort output delivered on EXHAUSTION —
	// the acceptance gate never passed, but rather than fail-with-nothing the
	// scheduler hands the last draft over with a caveat. AgentIterationCompleted
	// receivers (Saver) must treat it as a PARTIAL artefact (browsable, clearly
	// caveated), NOT a trusted final fact that enters memory recall.
	Partial bool

	// ToolCallsJSON is the marshalled tool-trace array for this iteration
	// (`[{name, input, output, error, ...}, ...]`), serialised by the
	// handler from whatever its inner agent.Loop returned. The scheduler
	// writes it verbatim into the agent_task_iterations.tool_calls jsonb
	// column when it records the iteration. Empty/nil means the handler
	// either didn't use tools or didn't bother to expose the trace.
	ToolCallsJSON json.RawMessage

	// PendingDeliveries are keyed task-program input items represented by this
	// iteration's Notify. The scheduler persists them only after Notify reaches
	// the user successfully; handler execution alone never marks delivery.
	PendingDeliveries []TaskDeliveryRef
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
	Embedder   EmbeddingProvider // nil = embedding disabled
	Registry   *ToolRegistry
	RoleTools  RoleToolQuerier
	ModelStore ModelConfigQuerier // model role → provider:model (nil = use Config.Models)
	Store      MessageStore       // session/message persistence for agent loops
	Prompts    PromptStore
	Users      UserStore      // nil = user lookup disabled
	Sessions   SessionQuerier // nil = session query disabled
	Logger     *slog.Logger
	DB         func(module string) (*sqlx.DB, error)
	UserID     uuid.UUID
	Config     *Config
	Deliveries TaskDeliveryLedger

	// DraftAutonomousTurn is late-copied from the live gateway on every
	// scheduler iteration. Nil means no interactive gateway is running.
	DraftAutonomousTurn AutonomousTurnDrafter

	// SelfAgentID is the Ship's own Fleet-issued agent id. Empty until
	// the first Fleet bootstrap call completes; used by delegate-strategy
	// handlers so the peer can route status callbacks back here.
	SelfAgentID func() string

	// ContextInjector builds per-request context (active notes, etc.) for the agent loop.
	// priorContext is the recent chat-thread excerpt; agent paths usually pass "".
	ContextInjector func(ctx context.Context, userID, message, priorContext string) string

	// ReflexPreparer returns structured context for the reflex pipeline.
	// priorContext is unused in agent paths (pass "") — only the chat gateway
	// has multi-turn thread state to forward.
	ReflexPreparer func(ctx context.Context, userID, message, priorContext string) *ReflexContext

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

// ctxKey is a private type for keys stored in request contexts so external
// packages can't collide with us. Used by ContextWithTaskID / ContextWith
// Iteration and read by tool handlers (e.g. browser_fetch) that need to
// know which agent_task iteration they're running on so they can persist
// fetched documents alongside the task instead of dropping the body
// through the 500-char ToolTrace.
type ctxKey int

const (
	ctxKeyTaskID ctxKey = iota
	ctxKeyIteration
)

// ContextWithTaskID returns ctx tagged with the running agent_task id.
// Scheduler stamps this before invoking handler.Run; tool handlers read
// it to attribute fetched documents and other per-task side-effects.
func ContextWithTaskID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKeyTaskID, id)
}

// TaskIDFromContext extracts the agent_task id stamped by the scheduler.
// Returns (uuid.Nil, false) when ctx has no task id — chat-mode tools and
// other non-task callers run this way; persistence side-effects should
// be a no-op in that case, not an error.
func TaskIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(ctxKeyTaskID).(uuid.UUID)
	return v, ok
}

// ContextWithIteration returns ctx tagged with the current iteration
// counter. Together with the task id this lets per-iteration audit
// records (e.g. agent_task_fetched_docs) be grouped by iteration so
// the evaluator can distinguish "fetched this iter" from "fetched
// three iters ago" — load-bearing for the recheck loop in Gate B'.
func ContextWithIteration(ctx context.Context, iter int) context.Context {
	return context.WithValue(ctx, ctxKeyIteration, iter)
}

// IterationFromContext extracts the iteration counter, defaulting to 0
// when absent. 0 is a real iteration value (the first one), so callers
// who care MUST check the boolean.
func IterationFromContext(ctx context.Context) (int, bool) {
	v, ok := ctx.Value(ctxKeyIteration).(int)
	return v, ok
}
