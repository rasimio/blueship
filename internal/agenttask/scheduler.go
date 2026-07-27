package agenttask

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/telemetry"
)

// jsonUnmarshal aliases stdlib json.Unmarshal so extractPeerTaskID can
// stay a one-liner without importing encoding/json at the call site.
var jsonUnmarshal = json.Unmarshal

// DefaultTaskTimeout is applied to tasks without an explicit deadline. A heavy
// research iteration (many browser_fetch + a long synthesis turn) can run well
// past 5 min; 10 gives it room to finish before the iteration ctx cancels the
// next LLM/DB call. Critical state writes are additionally detached from this
// ctx (agent.persistCtx) so they survive even when an iteration does overrun.
const DefaultTaskTimeout = 10 * time.Minute

// scheduleDueTolerance absorbs sub-second scheduler/DB timestamp skew. The
// polling loop otherwise arrives a few milliseconds before an exact duration,
// skips, and turns a 5-minute task into a 6-minute task. It is always clamped
// below the configured duration so short cadences cannot become continuously
// due.
const scheduleDueTolerance = time.Second

// Scheduler polls agent_tasks and dispatches handlers.
//
// Two dispatch paths:
//   - handler-keyed: AgentTask.Handler != "" — used by recurring jobs
//     (heartbeat, inner-thought, session-summary, etc.).
//   - strategy-keyed: AgentTask.Handler == "" — used by goal-style tasks
//     (direct / structured / delegate). Strategy maps to a handler in
//     strategyHandlers; if absent the task is failed.
type Scheduler struct {
	store            *core.AgentTaskStore
	handlers         map[string]core.AgentHandler
	strategyHandlers map[string]core.AgentHandler
	registry         *core.ToolRegistry
	// registryBuilder, when non-nil, builds a fresh per-task tool
	// registry bound to the task's user deps. Required for multi-tenant
	// hosts so per-tool closures (notes/memory/etc) read d.UserID =
	// task.UserID rather than the global Deps zero-value. Hosts that
	// don't need tenancy may leave it nil — the scheduler falls back to
	// the shared global registry.
	registryBuilder func(userDeps *core.Deps) *core.ToolRegistry
	msgStore        core.MessageStore
	deps            *core.Deps
	notify          func(ctx context.Context, userID uuid.UUID, text string) (core.TaskNotificationReceipt, error)
	notifyJournal   core.TaskNotificationJournal
	notifyTask      func(context.Context, uuid.UUID) (core.AgentTask, error)
	onStatusChange  func(ctx context.Context, task core.AgentTask)
	logger          *slog.Logger

	mu   sync.Mutex
	busy map[string]bool // task ID → currently executing
	// dailyAtWarned dedups the malformed-daily_at config warning per task:
	// the scheduler re-parses task config on every 60s tick, so without the
	// dedup a persistent typo would WARN forever. Lazily allocated under mu.
	// Warn-only state — losing it on restart just repeats one log line.
	dailyAtWarned map[string]bool
	taskWg        sync.WaitGroup // tracks in-flight executeTask goroutines
	// sem bounds how many tasks execute concurrently across the whole
	// scheduler — the back-to-back runner (runTask) holds a slot for a
	// one-off task's entire plan, so without a cap a burst of pending
	// one-offs would spawn unbounded LLM-heavy loops. Non-blocking: a task
	// that can't get a slot stays pending for a later tick.
	sem chan struct{}
}

// maxConcurrentTasks bounds simultaneous task execution (S2-a2 worker-pool
// lite). Conservative default; fairness/queueing is a later hardening slice.
const maxConcurrentTasks = 8

const (
	maxNotificationRetriesPerTick = 20
	maxHistoryProjectionsPerTick  = 50
	notificationAttemptTimeout    = 10 * time.Second
	defaultNotificationRetryDelay = time.Minute
)

// SetStatusCallback registers a function called after a task transitions
// to a terminal status (done/failed/canceled). Used to send A2A
// callbacks to the originating agent for delegate-strategy tasks. The
// callback runs in a goroutine; it must be self-contained and tolerant
// of nil DB / missing peer cache rows.
func (s *Scheduler) SetStatusCallback(cb func(ctx context.Context, task core.AgentTask)) {
	s.onStatusChange = cb
}

// SetRegistryBuilder installs a per-task tool-registry builder. Called
// by the host once after construction. See the field comment on
// Scheduler for the rationale.
func (s *Scheduler) SetRegistryBuilder(b func(userDeps *core.Deps) *core.ToolRegistry) {
	s.registryBuilder = b
}

// NewScheduler creates an agent task scheduler.
func NewScheduler(
	store *core.AgentTaskStore,
	handlers map[string]core.AgentHandler,
	strategyHandlers map[string]core.AgentHandler,
	registry *core.ToolRegistry,
	msgStore core.MessageStore,
	deps *core.Deps,
	notify func(ctx context.Context, userID uuid.UUID, text string) (core.TaskNotificationReceipt, error),
	logger *slog.Logger,
) *Scheduler {
	// AgentTaskStore is also the production notification journal. Keep the
	// dependency expressed as an interface so delivery ordering can be tested
	// without a database and older custom stores fail closed instead of sending
	// an unreserved keyed notification.
	notifyJournal, _ := any(store).(core.TaskNotificationJournal)
	return &Scheduler{
		store:            store,
		handlers:         handlers,
		strategyHandlers: strategyHandlers,
		registry:         registry,
		msgStore:         msgStore,
		deps:             deps,
		notify:           notify,
		notifyJournal:    notifyJournal,
		notifyTask:       store.Get,
		logger:           logger,
		busy:             make(map[string]bool),
		sem:              make(chan struct{}, maxConcurrentTasks),
	}
}

// Run executes one scheduler tick: picks up pending tasks and dispatches handlers.
// Called by scheduler.RunLoop every 60 seconds.
// WakeFromCallback processes a peer task ID from the callback channel.
// Called by RunLoopWithTrigger before Run().
func (s *Scheduler) WakeFromCallback(ctx context.Context, peerTaskID string) {
	if peerTaskID == "" {
		return
	}
	wokenID, err := s.store.WakePausedByPeerTask(ctx, peerTaskID)
	if err != nil {
		s.logger.Info("agent-tasks: no paused task for callback", "peer_task", peerTaskID)
		return
	}
	s.logger.Info("agent-tasks: woke paused task from callback",
		"task_id", wokenID, "peer_task", peerTaskID)
}

func (s *Scheduler) Run(ctx context.Context) error {
	s.logger.Info("agent-tasks: tick")
	// When a live gateway owns autonomous-history coordination, it drains and
	// projects under the same pair-scoped turn lock used by interactive Cortex
	// turns. Direct store reconciliation is reserved for headless deployments;
	// running it beside a gateway could mutate dialogue while Cortex reads it.
	if s.store != nil && (s.deps == nil || s.deps.EnsureAutonomousHistory == nil) {
		projected, err := s.store.ReconcileAutonomousHistory(ctx, maxHistoryProjectionsPerTick)
		if err != nil {
			s.logger.WarnContext(ctx, "agent-tasks: reconcile autonomous history failed", "error", err)
		} else if projected > 0 {
			s.logger.InfoContext(ctx, "agent-tasks: reconciled autonomous history", "count", projected)
		}
	}
	s.drainRetryableNotifications(ctx)

	// Auto-complete tasks that exhausted iterations but weren't marked done.
	// This is the one TERMINAL failure worth alerting the owner about — a
	// per-iteration handler error (below) just retries and is logged at WARN,
	// so transient infra/timeout/quota blips don't page anyone.
	for _, t := range s.store.CompleteExhausted(ctx) {
		s.logger.ErrorContext(ctx, "agent-tasks: task failed (iteration budget exhausted without completion)",
			"task_id", t.ID,
			"title", t.Title,
			"iteration", t.Iteration,
			"max_iterations", t.MaxIterations,
			"acceptance_feedback", t.AcceptanceFeedback)
		s.archiveTaskSession(ctx, t.ID, t.SoulID, t.SessionID)
		// Salvage: rather than leave the owner with nothing, deliver the best
		// draft with a caveat (a short pointer over the channel; the full
		// partial report lands as a /artefact page via the Saver hook).
		if strings.TrimSpace(t.Output) == "" {
			continue
		}
		if s.notify != nil {
			caveat := "⚠️ «" + t.Title + "» не дотянула до планки достоверности за отведённые итерации. " +
				"Сохранила лучший доступный черновик в /artefact — он частичный и непроверенный, перепроверь источники."
			notifyCtx := core.WithUserID(core.WithSoulID(ctx, t.SoulID), t.UserID)
			if _, err := s.notify(notifyCtx, t.UserID, caveat); err != nil {
				s.logger.WarnContext(ctx, "agent-tasks: exhausted-task caveat delivery failed",
					"task_id", t.ID, "user_id", t.UserID, "soul_id", t.SoulID, "error", err)
			}
		}
		if s.deps.AgentIterationCompletedHook != nil {
			task := core.AgentTask{ID: t.ID, Title: t.Title, UserID: t.UserID, SoulID: t.SoulID}
			res := core.IterationResult{Output: t.Output, IsFinal: true, Partial: true}
			go s.deps.AgentIterationCompletedHook(
				core.WithSoulID(context.Background(), t.SoulID), task, res)
		}
	}

	// Crash recovery: reset tasks stuck in 'running' for > 10 min.
	if n, err := s.store.ResetStale(ctx, 10*time.Minute); err != nil {
		s.logger.Warn("agent-tasks: reset stale failed", "error", err)
	} else if n > 0 {
		s.logger.Info("agent-tasks: reset stale tasks", "count", n)
	}

	// Watchdog: wake paused tasks that haven't received a callback in 30 min.
	if n, err := s.store.WakeStalePaused(ctx, 30*time.Minute); err != nil {
		s.logger.Warn("agent-tasks: wake stale paused failed", "error", err)
	} else if n > 0 {
		s.logger.Info("agent-tasks: woke stale paused tasks (lost callback?)", "count", n)
	}

	tasks, err := s.store.PendingTasks(ctx)
	if err != nil {
		s.logger.Error("agent-tasks: pending query failed", "error", err)
		return err
	}

	s.logger.Info("agent-tasks: pending", "count", len(tasks))

	for _, task := range tasks {
		handler, dispatchTag, ok := s.resolveHandler(task)
		if !ok {
			s.logger.Warn("agent-tasks: no dispatcher",
				"handler", task.Handler, "strategy", task.Strategy, "task_id", task.ID)
			reason := "no dispatcher: handler=" + task.Handler + " strategy=" + task.Strategy
			if err := s.store.Fail(ctx, task.ID, reason); err != nil {
				s.logger.Error("agent-tasks: fail update error", "error", err)
			}
			continue
		}
		if s.isBusy(task.ID.String()) {
			continue
		}

		// Check cron schedule for recurring tasks.
		if task.Schedule != nil && !s.shouldRunNow(task) {
			continue
		}

		// Quiet hours are a free scheduler gate. Keeping the task pending here
		// avoids stamping last_run_at, so the first tick after quiet hours runs
		// immediately instead of waiting another full cadence. The handler keeps
		// the same check as a second barrier against races/timezone changes.
		var schedulerConfig *core.Config
		if s.deps != nil {
			schedulerConfig = s.deps.Config
		}
		if taskProgramInQuietHours(ctx, task, schedulerConfig, time.Now()) {
			continue
		}

		// Not-before gate for tasks carrying config {"start_at": "ISO"}:
		// a delayed one-shot («сделай в 21:14») sleeps until its moment
		// without burning iterations. Same pre-dispatch placement as the
		// daily gate — a skip is free.
		if !s.startAtGateOpen(task, time.Now()) {
			continue
		}

		// Once-a-day local-hour gate for tasks carrying config
		// {"daily_at": {"hour": H}}. See dailyGateOpen for semantics and
		// placement rationale.
		if !s.dailyGateOpen(ctx, task, time.Now()) {
			continue
		}

		// Cadence guard for non-recurring tasks (e.g. periodic monitors
		// running on strategy=direct). Skips the tick without burning
		// an iteration if the task ran more recently than its cadence.
		if !s.cadenceElapsed(task) {
			continue
		}

		if !s.executionAllowed(ctx, task) {
			continue
		}
		// Acquire the local lease synchronously before spawning. The previous
		// check-then-set inside runTask left a window where two Run triggers
		// could launch the same task; TrySetRunning below remains the DB-level
		// fence across processes.
		if !s.trySetBusy(task.ID.String()) {
			continue
		}

		s.taskWg.Add(1)
		go s.runTask(ctx, task, handler, dispatchTag)
	}

	return nil
}

// runTask is the back-to-back runner (S2-a2 worker-pool lite). It holds a
// global concurrency slot + the busy lease for a task's whole run, then loops:
// one-off (schedule==nil) tasks advance to the next iteration IMMEDIATELY when
// the prior one was non-terminal — so a multi-step plan executes in seconds,
// not one step per 60s tick. Recurring tasks run exactly one iteration and
// return (fresh session per tick by design). Cancel / deadline / max_iterations
// are honoured by re-fetching the row each loop; progress persists per
// iteration inside executeTaskOnce, so a crash mid-loop just resumes next tick.
func (s *Scheduler) runTask(ctx context.Context, task core.AgentTask, handler core.AgentHandler, dispatchTag string) {
	defer s.taskWg.Done()
	defer s.setBusy(task.ID.String(), false)
	// Third agent between creation and execution: shape the task once
	// (skill from the catalog, iteration cap) before the first iteration.
	task = s.routeTaskSkill(ctx, task)
	// Bounded global concurrency for the heavy back-to-back ONE-OFF loops: take
	// a slot or leave the task pending for a later tick (no blocking — keeps the
	// scheduler loop responsive).
	//
	// Recurring ticks (heartbeat / reminders) are EXEMPT: they're lightweight,
	// time-critical, and run exactly one iteration. Making them compete for a
	// slot let a burst of heavy research one-offs starve reminders (a missed
	// heartbeat = a late nudge). They must always run on schedule.
	if task.Schedule == nil {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			return
		}
	}
	for {
		again := s.executeTaskOnce(ctx, task, handler, dispatchTag)
		if !again {
			return
		}
		// Re-fetch for the next iteration: executeTaskOnce set the row back to
		// pending + iteration++. Stop on terminal/paused/cancelled, exhausted
		// budget, passed deadline, or shutdown.
		next, err := s.store.Get(ctx, task.ID)
		if err != nil {
			s.logger.WarnContext(ctx, "agent-tasks: back-to-back refetch failed",
				"task_id", task.ID, "error", err)
			return
		}
		if next.Status != "pending" {
			return
		}
		if next.MaxIterations > 0 && next.Iteration >= next.MaxIterations {
			return
		}
		if next.Deadline != nil && !next.Deadline.After(time.Now()) {
			return
		}
		if ctx.Err() != nil {
			return
		}
		task = next
	}
}

// Wait blocks until all in-flight task goroutines complete.
// Called during graceful shutdown to ensure DB ops finish before connections close.
func (s *Scheduler) Wait() {
	s.taskWg.Wait()
}

// executeTaskOnce runs ONE iteration of a task. It returns again=true when the
// caller (runTask) should immediately run the next iteration back-to-back —
// i.e. a one-off (schedule==nil) task that produced a non-terminal result
// (continue or grounding-rejected). Terminal outcomes (done/failed/pause) and
// recurring tasks return false. The lease + concurrency slot are owned by
// runTask, not here.
func (s *Scheduler) executeTaskOnce(ctx context.Context, task core.AgentTask, handler core.AgentHandler, dispatchTag string) (again bool) {
	// Recheck at the actual execution boundary as well as at scheduler pickup:
	// a host policy can change after dispatch or between back-to-back steps.
	if !s.executionAllowed(ctx, task) {
		return false
	}

	// Tenant-attribute every write this iteration does. The task row
	// carries its own soul_id (denormalised in Phase A); thread it
	// through ctx so the handler, its tools, and the per-call DB ctxes
	// below all resolve the right soul.
	ctx = core.WithUserID(ctx, task.UserID)
	ctx = core.WithSoulID(ctx, task.SoulID)

	ctx, span := telemetry.StartTaskSpan(ctx, task.ID.String(), task.Handler, task.Strategy, dispatchTag, task.Iteration+1)
	defer span.End()

	s.logger.InfoContext(ctx, "agent-tasks: starting",
		"task_id", task.ID,
		"dispatch", dispatchTag,
		"title", task.Title,
		"iteration", task.Iteration+1,
	)

	claimed, claimErr := s.store.TrySetRunning(ctx, task.ID)
	if claimErr != nil {
		span.SetAttributes(attribute.String("agent_task.outcome", "set_running_failed"))
		telemetry.RecordError(span, claimErr)
		s.logger.ErrorContext(ctx, "agent-tasks: set running failed", "task_id", task.ID, "error", claimErr)
		return false
	}
	if !claimed {
		span.SetAttributes(attribute.String("agent_task.outcome", "claim_lost"))
		s.logger.DebugContext(ctx, "agent-tasks: task already claimed", "task_id", task.ID)
		return false
	}

	// Build per-task tool registry. When the host installed a
	// registryBuilder we rebuild every iteration so per-tool closures
	// see d.UserID = task.UserID (required for multi-tenant hosts —
	// without this, every tool that does `d.UserID.String()` queries
	// the wrong tenant and silently returns the global-deps-empty-uuid
	// owner's rows). Without a builder we fall back to the shared
	// global registry — fine for single-tenant agents.
	baseRegistry := s.registry
	if s.registryBuilder != nil {
		userDeps := s.deps.ForUser(task.UserID, "agent_task:"+task.ID.String(), false)
		baseRegistry = s.registryBuilder(userDeps)
	}
	requestedTools, hasRequestedTools, registryErr := taskRequestedTools(task)
	if registryErr == nil {
		baseRegistry, registryErr = s.composeSoulTaskRegistry(ctx, task, baseRegistry, requestedTools)
	} else {
		baseRegistry = core.NewToolRegistry()
	}
	registry := registryForTask(baseRegistry, handler.DefaultTools(), requestedTools, hasRequestedTools)

	agentDeps := core.AgentDeps{
		LLM:                 s.deps.LLM,
		Embedder:            s.deps.Embedder,
		Registry:            registry,
		RoleTools:           s.deps.RoleTools,
		ModelStore:          s.deps.ModelStore,
		Store:               s.msgStore,
		Prompts:             s.deps.Prompts,
		Users:               s.deps.Users,
		Sessions:            s.deps.Sessions,
		Logger:              s.logger,
		DB:                  s.deps.DB,
		UserID:              task.UserID,
		Config:              s.deps.Config,
		Deliveries:          s.store,
		SelfAgentID:         s.deps.SelfAgentID,
		DraftAutonomousTurn: s.deps.DraftAutonomousTurn,
		ContextInjector:     s.deps.ContextInjector,
		ReflexPreparer:      s.deps.ReflexPreparer,
		RuleEngine:          s.deps.RuleEngine,
	}

	// Apply deadline or default timeout.
	var cancel context.CancelFunc
	if task.Deadline != nil && task.Deadline.After(time.Now()) {
		ctx, cancel = context.WithDeadline(ctx, *task.Deadline)
	} else {
		ctx, cancel = context.WithTimeout(ctx, DefaultTaskTimeout)
	}
	defer cancel()

	// Tag the ctx with task id + iteration so per-task tool side-effects
	// (e.g. browser_fetch persisting to agent_task_fetched_docs) can
	// attribute themselves correctly. Chat-mode tool invocations don't
	// get this tag and skip persistence — non-task callers MUST remain
	// no-ops.
	ctx = core.ContextWithTaskID(ctx, task.ID)
	ctx = core.ContextWithIteration(ctx, task.Iteration+1)

	// Iteration-audit state: captured by the deferred RecordIteration
	// call below. Each branch (failed / pause / rejected / done / continue)
	// sets iterationOutcome and the relevant fields before returning, then
	// the deferred goroutine writes one row to agent_task_iterations. This
	// is the single source of truth for "what did this iteration do" — the
	// chat_messages session is destructive (compactor DELETE's), and
	// agent_tasks.progress is summarised to 500 chars.
	iterationStartedAt := time.Now()
	iterationOutcome := "continue"
	var iterationAcceptanceMet *bool
	var iterationAcceptanceReason string
	var iterationError string
	// Gate C audit state. Stays nil/empty when the iteration didn't run
	// grounding (recurring task / no criteria / no fetched docs / LLM
	// error during eval). When populated, RecordIteration writes the
	// triplet into agent_task_iterations for calibration + forensics.
	var iterationGroundedCount *int
	var iterationUngroundedCount *int
	var iterationGroundingVerdict json.RawMessage
	traceCtx := span.SpanContext()

	var result core.IterationResult
	var err error
	if registryErr != nil {
		err = registryErr
	} else {
		result, err = handler.Run(ctx, task, agentDeps)
	}

	// Fresh ctx per DB op. We use background-rooted (not the iteration
	// ctx, which may be cancelled on shutdown), but allocate per-call so
	// a long-running step in between — most notably the Gate C grounding
	// evaluator LLM call inside evaluateAcceptance — doesn't eat into the
	// budget of a downstream UPDATE. Pre-Gate-C this was a single shared
	// 10s dbCtx; that worked when acceptance was a quick check, but with
	// a 30-60s auditor LLM call in the middle the shared deadline blew
	// before reaching s.store.Complete / .UpdateProgress and every
	// finished research task logged "context deadline exceeded".
	newDBCtx := func() (context.Context, context.CancelFunc) {
		// Background-rooted (survives iteration-ctx cancel on shutdown)
		// but re-carries the soul so detached DB writes stay attributed.
		return context.WithTimeout(core.WithSoulID(context.Background(), task.SoulID), 10*time.Second)
	}

	// Audit-log writer fires last, after every branch above has had a
	// chance to set iterationOutcome / acceptance / error. Goroutine so
	// the DB write never blocks the scheduler tick.
	defer func() {
		rec := core.IterationRecord{
			TaskID:           task.ID,
			Iteration:        task.Iteration + 1,
			StartedAt:        iterationStartedAt,
			CompletedAt:      time.Now(),
			Outcome:          iterationOutcome,
			IsFinal:          result.IsFinal,
			AcceptanceMet:    iterationAcceptanceMet,
			AcceptanceReason: iterationAcceptanceReason,
			Output:           result.Output,
			Notify:           result.Notify,
			ToolCalls:        result.ToolCallsJSON,
			Progress:         result.Progress,
			Error:            iterationError,
			TraceID:          traceCtx.TraceID().String(),
			SpanID:           traceCtx.SpanID().String(),
			GroundedCount:    iterationGroundedCount,
			UngroundedCount:  iterationUngroundedCount,
			GroundingVerdict: iterationGroundingVerdict,
		}
		go func() {
			recCtx, recCancel := context.WithTimeout(core.WithSoulID(context.Background(), task.SoulID), 10*time.Second)
			defer recCancel()
			if err := s.store.RecordIteration(recCtx, rec); err != nil {
				s.logger.WarnContext(recCtx, "agent-tasks: record iteration failed",
					"task_id", task.ID, "iteration", rec.Iteration, "error", err)
			}
		}()
	}()

	if err != nil {
		iterationOutcome = "failed"
		iterationError = err.Error()
		span.SetAttributes(attribute.String("agent_task.outcome", "failed"))
		telemetry.RecordError(span, err)
		// WARN, not ERROR: this iteration failed but SetPending retries it, and
		// most causes are transient + self-healing (DB pool/timeout blips, an
		// LLM 429 usage-limit that clears on quota reset). Alerting on each one
		// is noise — the owner is paged only when the task truly dies
		// (CompleteExhausted, logged at ERROR above). Still logged for forensics.
		s.logger.WarnContext(ctx, "agent-tasks: iteration failed, will retry",
			"task_id", task.ID,
			"handler", task.Handler,
			"error", err,
		)
		dbCtx, dbCancel := newDBCtx()
		defer dbCancel()
		if fErr := s.store.SetPending(dbCtx, task.ID); fErr != nil {
			s.logger.ErrorContext(ctx, "agent-tasks: reset after fail error", "error", fErr)
		}
		// Handler error: SetPending leaves it for a retry, but don't hammer it
		// back-to-back — let the next tick pick it up (avoids a hot crash loop).
		return false
	}

	// Defer the iteration-completed hook so it fires at function return,
	// AFTER every branch has had a chance to mutate result.IsFinal.
	// Closure captures `result` by reference (it's a named return-style
	// local), so the hook sees the final state — IsFinal=true only when
	// the acceptance gate has actually approved a Done-claim, not on
	// every Done-from-handler intermediate. Without this delay, a Saver
	// that gates on result.IsFinal would still fire on rejected drafts
	// because Done was already true at hook time. Goroutine inside so a
	// slow consumer doesn't stall executeTask completion.
	defer func() {
		if s.deps.AgentIterationCompletedHook != nil {
			go s.deps.AgentIterationCompletedHook(core.WithSoulID(context.Background(), task.SoulID), task, result)
		}
	}()

	// Don't fire s.notify here — gate it on each branch's outcome below.
	// Each branch decides
	// whether to actually push to the user. Critical for research-style
	// agent_tasks with strict acceptance criteria: handler returns Done
	// with a long Output, evaluator rejects (0 URLs, etc.), and we used
	// to push the rejected draft to chat anyway because notify ran above
	// the gate. On 2026-05-10 task 988183c5 leaked a 6.5K-char fake
	// "AWM final report" to Telegram on iter 15 right before the gate
	// failed it for missing citations — exactly this bug.
	deliverNotify := func() (taskNotificationOutcome, error) {
		notifyCtx, notifyCancel := newDBCtx()
		defer notifyCancel()
		outcome, err := deliverTaskNotification(
			notifyCtx, s.notify, s.notifyJournal,
			task.ID, task.UserID, result.Notify, result.PendingDeliveries,
		)
		if err != nil {
			s.logger.WarnContext(ctx, "agent-tasks: notify delivery failed",
				"task_id", task.ID, "error", err)
		}
		result.Notified = outcome.Delivered
		return outcome, err
	}
	stopForNotificationFailure := func(disposition notificationFailureDisposition, notifyErr error) bool {
		if disposition == notificationFailureProceed {
			return false
		}
		iterationOutcome = "notify_failed"
		iterationError = notifyErr.Error()
		result.IsFinal = false
		dbCtx, dbCancel := newDBCtx()
		defer dbCancel()
		if err := s.store.SetPendingForNotificationRetry(dbCtx, task.ID); err != nil {
			s.logger.ErrorContext(ctx, "agent-tasks: notification retry requeue failed", "error", err)
		}
		return true
	}

	if result.Pause {
		iterationOutcome = "pause"
		// Pause carries explicit milestone notifications (handler sets
		// Notify only when there's something user-actionable). Push.
		notifyOutcome, notifyErr := deliverNotify()
		if stopForNotificationFailure(
			classifyNotificationFailure(task, result, notifyOutcome, notifyErr, false),
			notifyErr,
		) {
			return false
		}
		span.SetAttributes(
			attribute.String("agent_task.outcome", "paused"),
			attribute.Bool("agent_task.notified", notifyOutcome.Delivered),
		)
		peerTaskID := extractPeerTaskID(result.Progress)
		if peerTaskID != "" {
			span.SetAttributes(attribute.String("agent_task.peer_task_id", peerTaskID))
		}
		s.logger.InfoContext(ctx, "agent-tasks: paused (waiting for callback)",
			"task_id", task.ID,
			"handler", task.Handler,
			"iteration", task.Iteration+1,
			"peer_task_id", peerTaskID,
		)
		dbCtx, dbCancel := newDBCtx()
		defer dbCancel()
		if err := s.store.PauseTask(dbCtx, task.ID, result.Progress); err != nil {
			s.logger.ErrorContext(ctx, "agent-tasks: pause update error", "error", err)
		}
		return false
	}

	if result.Done {
		// Acceptance criteria gate: if the task carries criteria and the
		// handler claims done on a non-recurring strategy, ask the LLM
		// to verify. Recurring jobs (Schedule != nil) always complete on
		// the handler's word.
		if task.Schedule == nil && task.AcceptanceCriteria != nil &&
			strings.TrimSpace(*task.AcceptanceCriteria) != "" {
			verdict := evaluateAcceptance(ctx, agentDeps, task, result.Output, result.ToolCallsJSON)
			met := verdict.Met
			iterationAcceptanceMet = &met
			iterationAcceptanceReason = verdict.Reason
			// Capture Gate C output (always — shadow mode runs even on
			// pass paths so calibration sees the full distribution).
			if verdict.Grounding != nil {
				g := verdict.Grounding.GroundedCount
				u := verdict.Grounding.UngroundedCount
				iterationGroundedCount = &g
				iterationUngroundedCount = &u
				if blob, err := json.Marshal(verdict.Grounding); err == nil {
					iterationGroundingVerdict = blob
				}
			}
			if !verdict.Met {
				iterationOutcome = "rejected"
				// Recheck URLs only carry over when Gate C identified
				// specific URLs the next iteration must re-verify. Other
				// rejection paths (coverage gap from the LLM evaluator,
				// hard URL-count gate) don't bind to a URL list and the
				// store call collapses to plain UpdateProgress.
				var recheckURLs []string
				if verdict.Grounding != nil && len(verdict.Grounding.RecheckURLs) > 0 {
					recheckURLs = verdict.Grounding.RecheckURLs
				}
				span.SetAttributes(
					attribute.String("agent_task.outcome", "criteria_not_met"),
					attribute.Bool("agent_task.acceptance_met", false),
					attribute.String("agent_task.acceptance_reason", verdict.Reason),
					attribute.Int("agent_task.output_size_bytes", len(result.Output)),
					attribute.Int("agent_task.recheck_url_count", len(recheckURLs)),
				)
				s.logger.InfoContext(ctx, "agent-tasks: criteria not met, continuing",
					"task_id", task.ID, "reason", verdict.Reason,
					"recheck_urls", len(recheckURLs))
				// Encode reason into progress so the next iteration
				// sees what the reviewer flagged.
				progressWithReason := rejectionProgress(task.Progress, result.Progress, verdict.Reason)
				dbCtx, dbCancel := newDBCtx()
				defer dbCancel()
				if err := s.store.UpdateProgressWithRecheck(dbCtx, task.ID, progressWithReason, recheckURLs); err != nil {
					s.logger.ErrorContext(ctx, "agent-tasks: progress update error", "error", err)
				}
				// Non-terminal: a one-off retries immediately (back-to-back).
				return task.Schedule == nil
			}
		}

		// Acceptance passed (or no criteria) → this Done-claim IS the
		// final terminal state. Mark the result so the deferred
		// AgentIterationCompletedHook can persist a single research_report
		// instead of one per rejected draft. Also: only NOW push the
		// finished output to the user; pre-acceptance notify would leak
		// rejected drafts.
		iterationOutcome = "done"
		result.IsFinal = true
		notifyOutcome, notifyErr := deliverNotify()
		if stopForNotificationFailure(
			classifyNotificationFailure(task, result, notifyOutcome, notifyErr, true),
			notifyErr,
		) {
			return false
		}
		span.SetAttributes(
			attribute.String("agent_task.outcome", "done"),
			attribute.Int("agent_task.output_size_bytes", len(result.Output)),
			attribute.Bool("agent_task.notified", notifyOutcome.Delivered),
		)
		if task.AcceptanceCriteria != nil && strings.TrimSpace(*task.AcceptanceCriteria) != "" {
			span.SetAttributes(attribute.Bool("agent_task.acceptance_met", true))
		}
		s.logger.InfoContext(ctx, "agent-tasks: completed",
			"task_id", task.ID,
			"dispatch", dispatchTag,
			"output_size_bytes", len(result.Output),
			"output_preview", outputPreview(result.Output),
			"notified", notifyOutcome.Delivered,
		)
		completeCtx, completeCancel := newDBCtx()
		completeErr := s.store.Complete(completeCtx, task.ID, result.Output)
		if completeErr != nil {
			s.logger.ErrorContext(ctx, "agent-tasks: complete update error", "error", completeErr)
		}
		completeCancel()
		if completeErr == nil && task.Schedule == nil {
			sessionID := sessionIDFromProgress(result.Progress)
			if sessionID == "" {
				sessionID = sessionIDFromProgress(task.Progress)
			}
			s.archiveTaskSession(ctx, task.ID, task.SoulID, sessionID)
		}
		// Recurring tasks: reset for next run.
		if task.Schedule != nil {
			resetCtx, resetCancel := newDBCtx()
			if err := s.store.ResetForNextRun(resetCtx, task.ID); err != nil {
				s.logger.ErrorContext(ctx, "agent-tasks: reset for next run error", "error", err)
			}
			resetCancel()
		}
		// Notify origin agent (delegate-strategy callback). Non-recurring
		// only — recurring tasks never originate from a peer.
		if task.Schedule == nil && s.onStatusChange != nil {
			task.Status = "done"
			task.Result = &result.Output
			go s.onStatusChange(core.WithSoulID(context.Background(), task.SoulID), task)
		}
		return false
	} else {
		// Mid-task iteration. Push only when the handler explicitly
		// flagged something user-relevant via Notify (milestone, blocker)
		// — random in-progress output is noise, not a message.
		notifyOutcome, notifyErr := deliverNotify()
		if stopForNotificationFailure(
			classifyNotificationFailure(task, result, notifyOutcome, notifyErr, false),
			notifyErr,
		) {
			return false
		}
		span.SetAttributes(
			attribute.String("agent_task.outcome", "iteration_done"),
			attribute.Bool("agent_task.notified", notifyOutcome.Delivered),
		)
		s.logger.InfoContext(ctx, "agent-tasks: iteration done",
			"task_id", task.ID,
			"handler", task.Handler,
			"iteration", task.Iteration+1,
			"notified", notifyOutcome.Delivered,
		)
		dbCtx, dbCancel := newDBCtx()
		defer dbCancel()
		if err := s.store.UpdateProgress(dbCtx, task.ID, result.Progress); err != nil {
			s.logger.ErrorContext(ctx, "agent-tasks: progress update error", "error", err)
		}
		// Non-terminal mid-task iteration: a one-off advances to the next step
		// immediately (back-to-back); recurring waits for the next tick.
		return task.Schedule == nil
	}
}

type notificationFailureDisposition uint8

const (
	notificationFailureProceed notificationFailureDisposition = iota
	notificationFailureRetry
)

const maxNotificationConfirmAttempts = 3

func classifyNotificationFailure(
	task core.AgentTask,
	result core.IterationResult,
	outcome taskNotificationOutcome,
	notifyErr error,
	retryUnkeyed bool,
) notificationFailureDisposition {
	if notifyErr == nil {
		return notificationFailureProceed
	}
	keyed := len(result.PendingDeliveries) > 0
	if keyed {
		// The handler has already run and may have performed external MCP
		// actions. Even admission failures cannot safely rerun the task program;
		// only a persisted journal intent may retry its immutable message.
		return notificationFailureProceed
	}
	if retryUnkeyed && task.Schedule != nil && !outcome.Delivered {
		return notificationFailureRetry
	}
	return notificationFailureProceed
}

type taskNotificationOutcome struct {
	// Handled means the task state may advance without rerunning the handler.
	// Every keyed post-handler outcome is handled because the handler may already
	// have performed external actions; this does not prove journal admission or
	// user-visible delivery.
	Handled bool
	// Delivered is true only when the transport returned success. Confirmation
	// persistence may still have failed; the reservation remains the retry fence.
	Delivered bool
}

func deliverTaskNotification(
	ctx context.Context,
	notify func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error),
	journal core.TaskNotificationJournal,
	taskID, userID uuid.UUID,
	text string,
	refs []core.TaskDeliveryRef,
) (taskNotificationOutcome, error) {
	if strings.TrimSpace(text) == "" || strings.TrimSpace(text) == "[no-op]" {
		if len(refs) > 0 {
			return taskNotificationOutcome{Handled: true}, fmt.Errorf("keyed notification has no deliverable text")
		}
		return taskNotificationOutcome{Handled: true}, nil
	}
	if len(refs) == 0 {
		if notify == nil {
			return taskNotificationOutcome{}, fmt.Errorf("notify: sender unavailable")
		}
		if _, err := notify(ctx, userID, text); err != nil {
			return taskNotificationOutcome{}, fmt.Errorf("notify: %w", err)
		}
		return taskNotificationOutcome{Handled: true, Delivered: true}, nil
	}
	if journal == nil {
		return taskNotificationOutcome{Handled: true}, fmt.Errorf("begin notification attempt: journal unavailable")
	}
	attemptID, created, err := journal.BeginNotificationAttempt(ctx, taskID, userID, text, refs)
	if err != nil {
		return taskNotificationOutcome{Handled: true}, fmt.Errorf("begin notification attempt: %w", err)
	}
	if !created {
		// The immutable intent already owns these refs. Its state is advanced by
		// the outbox worker, never by rerunning the task program.
		return taskNotificationOutcome{Handled: true}, nil
	}
	if notify == nil {
		notifyErr := core.DefinitelyNotSent(fmt.Errorf("sender unavailable"))
		_, resolveErr := resolveTaskNotificationAttempt(ctx, journal, attemptID, core.TaskNotificationReceipt{}, notifyErr)
		return taskNotificationOutcome{Handled: true}, resolveErr
	}

	// Begin uses the scheduler's short DB budget. Give the one permitted
	// provider request its own full deadline so a slow reservation query cannot
	// consume the transport window. WithoutCancel preserves task/user/soul
	// values while removing the spent parent deadline.
	transportCtx, transportCancel := context.WithTimeout(context.WithoutCancel(ctx), notificationAttemptTimeout)
	defer transportCancel()
	transportCtx = core.ContextWithSingleAttemptNotification(transportCtx)
	transportCtx = core.ContextWithNotificationAttemptID(transportCtx, attemptID)
	receipt, notifyErr := notify(transportCtx, userID, text)
	delivered, resolveErr := resolveTaskNotificationAttempt(ctx, journal, attemptID, receipt, notifyErr)
	return taskNotificationOutcome{Handled: true, Delivered: delivered}, resolveErr
}

// resolveTaskNotificationAttempt persists the outcome of exactly one provider
// call. Once Begin/Claim has returned an intent, every outcome is handled by
// the journal: task-program execution is never used as the retry mechanism.
func resolveTaskNotificationAttempt(
	ctx context.Context,
	journal core.TaskNotificationJournal,
	attemptID uuid.UUID,
	receipt core.TaskNotificationReceipt,
	notifyErr error,
) (bool, error) {
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notificationAttemptTimeout)
	defer cancel()
	if notifyErr == nil {
		var confirmErr error
		for attempt := 0; attempt < maxNotificationConfirmAttempts; attempt++ {
			confirmErr = journal.ConfirmNotificationAttempt(finalizeCtx, attemptID, receipt)
			if confirmErr == nil {
				return true, nil
			}
			if finalizeCtx.Err() != nil {
				break
			}
		}
		return true, fmt.Errorf("confirm notification attempt after %d tries: %w",
			maxNotificationConfirmAttempts, confirmErr)
	}
	if core.IsPermanentlyNotSent(notifyErr) {
		if err := journal.RejectNotificationAttempt(finalizeCtx, attemptID, notifyErr.Error()); err != nil {
			return false, fmt.Errorf("notify permanently not sent: %w; reject notification attempt: %v", notifyErr, err)
		}
		return false, fmt.Errorf("notify permanently not sent: %w", notifyErr)
	}
	if core.IsDefinitelyNotSent(notifyErr) {
		delay, ok := core.NotificationRetryDelay(notifyErr)
		if !ok || delay < defaultNotificationRetryDelay {
			delay = defaultNotificationRetryDelay
		}
		retryAt := time.Now().Add(delay)
		if err := journal.DeferNotificationAttempt(finalizeCtx, attemptID, notifyErr.Error(), retryAt); err != nil {
			return false, fmt.Errorf("notify definitely not sent: %w; defer notification attempt: %v", notifyErr, err)
		}
		return false, fmt.Errorf("notify definitely not sent: %w", notifyErr)
	}
	if err := journal.MarkNotificationUncertain(finalizeCtx, attemptID, notifyErr.Error()); err != nil {
		return false, fmt.Errorf("notify uncertain: %w; mark notification uncertain: %v", notifyErr, err)
	}
	return false, fmt.Errorf("notify uncertain: %w", notifyErr)
}

// drainRetryableNotifications retries immutable journaled text before running
// any task programs. A single tick is bounded so a large provider backlog
// cannot starve normal task scheduling.
func (s *Scheduler) drainRetryableNotifications(ctx context.Context) {
	if s.notifyJournal == nil {
		return
	}
	claimBefore := time.Now()
	for i := 0; i < maxNotificationRetriesPerTick; i++ {
		claimCtx, cancel := context.WithTimeout(ctx, notificationAttemptTimeout)
		intent, err := s.notifyJournal.ClaimRetryableNotification(claimCtx, claimBefore)
		cancel()
		if err != nil {
			s.logger.WarnContext(ctx, "agent-tasks: claim retryable notification failed", "error", err)
			return
		}
		if intent == nil {
			return
		}
		if err := s.retryTaskNotification(ctx, *intent); err != nil {
			s.logger.WarnContext(ctx, "agent-tasks: retry notification failed",
				"attempt_id", intent.ID, "task_id", intent.TaskID, "error", err)
		}
	}
}

func (s *Scheduler) retryTaskNotification(ctx context.Context, intent core.TaskNotificationIntent) error {
	lookup := s.notifyTask
	if lookup == nil && s.store != nil {
		lookup = s.store.Get
	}
	if lookup == nil {
		_, err := resolveTaskNotificationAttempt(ctx, s.notifyJournal, intent.ID, core.TaskNotificationReceipt{},
			core.DefinitelyNotSent(fmt.Errorf("task lookup unavailable")))
		return err
	}
	lookupCtx, lookupCancel := context.WithTimeout(ctx, notificationAttemptTimeout)
	task, err := lookup(lookupCtx, intent.TaskID)
	lookupCancel()
	if err != nil {
		_, resolveErr := resolveTaskNotificationAttempt(ctx, s.notifyJournal, intent.ID, core.TaskNotificationReceipt{},
			core.DefinitelyNotSent(fmt.Errorf("lookup task %s: %w", intent.TaskID, err)))
		return resolveErr
	}
	if task.UserID != intent.UserID {
		_, resolveErr := resolveTaskNotificationAttempt(core.WithSoulID(ctx, task.SoulID), s.notifyJournal, intent.ID,
			core.TaskNotificationReceipt{}, core.PermanentlyNotSent(fmt.Errorf(
				"notification user %s does not match task user %s", intent.UserID, task.UserID)))
		return resolveErr
	}
	if s.notify == nil {
		baseCtx := core.WithUserID(core.WithSoulID(ctx, task.SoulID), task.UserID)
		_, err := resolveTaskNotificationAttempt(baseCtx, s.notifyJournal, intent.ID,
			core.TaskNotificationReceipt{}, core.DefinitelyNotSent(fmt.Errorf("sender unavailable")))
		return err
	}
	transportCtx := core.WithSoulID(context.WithoutCancel(ctx), task.SoulID)
	transportCtx = core.WithUserID(transportCtx, task.UserID)
	transportCtx = core.ContextWithSingleAttemptNotification(transportCtx)
	transportCtx = core.ContextWithNotificationAttemptID(transportCtx, intent.ID)
	transportCtx, transportCancel := context.WithTimeout(transportCtx, notificationAttemptTimeout)
	defer transportCancel()
	receipt, notifyErr := s.notify(transportCtx, intent.UserID, intent.Text)
	delivered, resolveErr := resolveTaskNotificationAttempt(transportCtx, s.notifyJournal, intent.ID, receipt, notifyErr)
	if delivered && resolveErr == nil && s.deps != nil && s.deps.AgentIterationCompletedHook != nil {
		hookCtx := core.WithUserID(core.WithSoulID(context.Background(), task.SoulID), task.UserID)
		result := core.IterationResult{Notified: true, PendingDeliveries: intent.Refs}
		go s.deps.AgentIterationCompletedHook(hookCtx, task, result)
	}
	return resolveErr
}

// taskRequestedTools returns the task-owned side of tool selection. A typed
// task program is authoritative when present; AgentTask.Tools remains the
// legacy source for every task without a program. present-but-invalid programs
// intentionally request an empty registry — the background handler will
// return the validation error before executing anything.
func taskRequestedTools(task core.AgentTask) ([]string, bool, error) {
	if program, present, err := core.ParseTaskProgram(task.Config); present {
		if err != nil {
			return nil, true, fmt.Errorf("task_program: %w", err)
		}
		return program.RequestedTools(), true, nil
	}
	if len(task.Tools) == 0 {
		return nil, false, nil
	}
	return []string(task.Tools), true, nil
}

// registryForTask applies two independent restrictions in order:
//
//  1. handlerDefaults is the hard role/handler ceiling;
//  2. requested is the task-specific narrowing.
//
// Subsetting the already-ceilinged registry is important: taking only the
// task list first let a persisted task expand beyond Background.DefaultTools.
func registryForTask(base *core.ToolRegistry, handlerDefaults, requested []string, hasRequested bool) *core.ToolRegistry {
	allowed := base
	if len(handlerDefaults) > 0 {
		allowed = base.SubsetForNames(handlerDefaults)
	}
	if hasRequested {
		allowed = allowed.SubsetForNames(requested)
	}
	return allowed
}

// composeSoulTaskRegistry adds the task owner's live MCP tools to its native
// registry and applies the same provider/override policy used by interactive
// turns. Unlike chat, a background policy lookup error fails closed: an
// unattended task must not gain tools because its policy store is unavailable.
func (s *Scheduler) composeSoulTaskRegistry(ctx context.Context, task core.AgentTask, base *core.ToolRegistry, requestedTools []string) (*core.ToolRegistry, error) {
	if base == nil {
		base = core.NewToolRegistry()
	}
	if s.deps == nil || s.deps.Config == nil {
		return base, nil
	}
	cfg := s.deps.Config
	registry := base
	// MCP discovery can cold-connect external servers. Do it only when the
	// task explicitly requested a concrete MCP tool; a broad host ceiling such
	// as peer:mcp must not wake every server for a native-only heartbeat.
	if cfg.MCPSource != nil && requestsMCPTool(requestedTools) && !taskProgramInQuietHours(ctx, task, cfg, time.Now()) {
		if mcpTools := cfg.MCPSource.ToolsForSoul(ctx, task.SoulID); len(mcpTools) > 0 {
			registry = base.Clone()
			for _, tool := range mcpTools {
				registry.RegisterRemote(tool.Name, tool.Description, tool.Schema, core.ToolModeSync, "mcp", tool.Handler)
			}
		}
	}

	if task.SoulID == uuid.Nil || cfg.Gateway.ResolveSoulToolPolicy == nil {
		return registry, nil
	}
	overrides, providers, err := cfg.Gateway.ResolveSoulToolPolicy(ctx, task.SoulID)
	if err != nil {
		return core.NewToolRegistry(), fmt.Errorf("background tool policy for soul %s: %w", task.SoulID, err)
	}
	connected := make(map[string]bool, len(providers))
	for _, provider := range providers {
		connected[provider] = true
	}

	allowed := make([]string, 0, registry.Count())
	for _, definition := range registry.Definitions() {
		name := definition.Name
		meta := cfg.ToolMeta[name]
		if meta.Provider != "" && !connected[meta.Provider] {
			continue
		}
		if meta.Core {
			allowed = append(allowed, name)
			continue
		}
		if enabled, exists := overrides[name]; exists {
			if enabled {
				allowed = append(allowed, name)
			}
			continue
		}
		allowed = append(allowed, name)
	}
	return registry.SubsetForNames(allowed), nil
}

func requestsMCPTool(tools []string) bool {
	for _, tool := range tools {
		if strings.HasPrefix(strings.TrimSpace(tool), "mcp__") {
			return true
		}
	}
	return false
}

func taskProgramInQuietHours(ctx context.Context, task core.AgentTask, cfg *core.Config, now time.Time) bool {
	program, present, err := core.ParseTaskProgram(task.Config)
	if err != nil || !present || program.QuietHours == nil {
		return false
	}
	loc := time.UTC
	if cfg != nil {
		if configured, err := time.LoadLocation(cfg.Timezone); err == nil {
			loc = configured
		}
		loc = cfg.Gateway.TimezoneFor(core.WithSoulID(ctx, task.SoulID), loc)
	}
	hour := now.In(loc).Hour()
	from, to := program.QuietHours.FromHour, program.QuietHours.ToHour
	if from < to {
		return hour >= from && hour < to
	}
	return hour >= from || hour < to
}

func (s *Scheduler) executionAllowed(ctx context.Context, task core.AgentTask) bool {
	if s.deps.AuthorizeExecution == nil {
		return true
	}
	decision, err := s.deps.AuthorizeExecution(ctx, core.ExecutionRequest{
		UserID: task.UserID, SoulID: task.SoulID,
		Kind: core.ExecutionBackground, Transport: "agent_task",
	})
	if err != nil {
		s.logger.WarnContext(ctx, "agent-tasks: execution authorization failed",
			"task_id", task.ID, "user_id", task.UserID, "error", err)
		return false
	}
	if !decision.Allowed {
		s.logger.DebugContext(ctx, "agent-tasks: execution denied",
			"task_id", task.ID, "user_id", task.UserID, "reason", decision.Reason)
		return false
	}
	return true
}

// resolveHandler picks the right executor for a task, preferring the
// handler-keyed map (recurring jobs) and falling back to the strategy-
// keyed map (goal-style direct/structured/delegate). Returns the
// dispatch tag for diagnostics.
func (s *Scheduler) resolveHandler(task core.AgentTask) (core.AgentHandler, string, bool) {
	if task.Handler != "" {
		h, ok := s.handlers[task.Handler]
		return h, "handler:" + task.Handler, ok
	}
	if task.Strategy != "" && task.Strategy != core.StrategyRecurring {
		h, ok := s.strategyHandlers[task.Strategy]
		return h, "strategy:" + task.Strategy, ok
	}
	return nil, "", false
}

// cadenceElapsed returns true when the task is allowed to tick — either
// because no cadence is set, the cadence is unparseable (treated as
// "fire freely" so a typo doesn't strand a task), or enough time has
// passed since the last run. Unlike Schedule, Cadence applies to
// non-recurring tasks: it only rate-limits ticks, doesn't drive them.
func (s *Scheduler) cadenceElapsed(task core.AgentTask) bool {
	if task.Cadence == nil || *task.Cadence == "" {
		return true
	}
	d, err := time.ParseDuration(*task.Cadence)
	if err != nil {
		s.logger.Warn("agent-tasks: invalid cadence duration",
			"cadence", *task.Cadence, "task_id", task.ID)
		return true
	}
	if task.LastRunAt == nil {
		return true
	}
	return durationDue(time.Since(*task.LastRunAt), d)
}

// shouldRunNow checks if a recurring task should run based on its schedule.
// MVP: schedule is a Go duration string (e.g. "24h", "30m").
// TODO: cron expression support.
func (s *Scheduler) shouldRunNow(task core.AgentTask) bool {
	if task.Schedule == nil {
		return true
	}
	d, err := time.ParseDuration(*task.Schedule)
	if err != nil {
		s.logger.Warn("agent-tasks: invalid schedule duration", "schedule", *task.Schedule, "task_id", task.ID)
		return false
	}
	if task.LastRunAt == nil {
		return true
	}
	return durationDue(time.Since(*task.LastRunAt), d)
}

func durationDue(elapsed, interval time.Duration) bool {
	if interval <= 0 {
		return elapsed >= interval
	}
	tolerance := scheduleDueTolerance
	if tolerance >= interval {
		tolerance = interval / 2
	}
	return elapsed >= interval-tolerance
}

// startAtGateOpen enforces the optional not-before gate a task may carry
// in its config:
//
//	{"start_at": "2026-07-15T21:14:00+02:00"}
//
// Ticks before the moment are skipped exactly like a schedule/cadence
// miss: pre-dispatch, no iteration burned, last_run_at untouched. The
// ISO value carries its own timezone, so no owner-tz resolution is
// needed. Malformed values are treated as absent (a typo must not
// strand the task) and WARN-logged once per task.
func (s *Scheduler) startAtGateOpen(task core.AgentTask, now time.Time) bool {
	if len(task.Config) == 0 {
		return true
	}
	var cfg struct {
		StartAt string `json:"start_at"`
	}
	if json.Unmarshal(task.Config, &cfg) != nil || strings.TrimSpace(cfg.StartAt) == "" {
		return true
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(cfg.StartAt))
	if err != nil {
		id := task.ID.String() + ":start_at"
		s.mu.Lock()
		if s.dailyAtWarned == nil {
			s.dailyAtWarned = make(map[string]bool)
		}
		seen := s.dailyAtWarned[id]
		s.dailyAtWarned[id] = true
		s.mu.Unlock()
		if !seen {
			s.logger.Warn("agent-tasks: malformed start_at, gate ignored",
				"task_id", task.ID.String(), "start_at", cfg.StartAt)
		}
		return true
	}
	return !now.Before(at)
}

// dailyGateOpen enforces the optional once-a-day time-of-day gate a task
// may carry in its config:
//
//	{"daily_at": {"hour": 9}}
//
// A task with the key dispatches at most once per local day, and only
// while the owner's local hour equals hour (the whole 60-minute window)
// — e.g. a recurring "30m" digest that should fire once every morning.
// Ticks outside the window, or inside it once the task already dispatched
// that local day, are skipped exactly like a schedule/cadence miss:
// before SetRunning, so no handler run, no iteration burned, and
// last_run_at untouched — a skip never marks the day as done.
//
// The gate lives here, at the scheduler's dispatch decision next to the
// schedule/cadence guards, rather than inside the handler, because
// (a) only a pre-dispatch skip is free — by handler time SetRunning has
// already stamped last_run_at and the run counts as the day's dispatch;
// (b) the owner's timezone IS resolvable at this point: task rows carry
// soul_id, so the same soul-pinned Gateway.ResolveTimezone hook the
// background handler uses for [current_datetime] applies; and (c) the
// once-per-day guard derives from task.last_run_at — persisted by
// SetRunning on every dispatch and preserved by ResetForNextRun — so it
// survives daemon restarts with no in-memory state.
//
// Tasks without daily_at are unaffected: the gate is always open and the
// existing schedule/cadence checks alone decide the tick.
func (s *Scheduler) dailyGateOpen(ctx context.Context, task core.AgentTask, now time.Time) bool {
	hour, ok := s.dailyAtHour(task)
	if !ok {
		return true
	}
	// Owner-local wall clock: per-soul tz via the gateway hook, falling
	// back to the configured process timezone (server tz) when the hook
	// is absent or the soul has none set — the same ladder as the
	// current_time tool and the background handler's [current_datetime].
	loc := time.UTC
	if s.deps != nil && s.deps.Config != nil {
		if l, err := time.LoadLocation(s.deps.Config.Timezone); err == nil {
			loc = l
		}
		loc = s.deps.Config.Gateway.TimezoneFor(core.WithSoulID(ctx, task.SoulID), loc)
	}
	local := now.In(loc)
	if local.Hour() != hour {
		return false
	}
	if task.LastRunAt == nil {
		return true
	}
	// Already dispatched today (owner-local date) → closed until tomorrow.
	ly, lm, ld := task.LastRunAt.In(loc).Date()
	ny, nm, nd := local.Date()
	return ly != ny || lm != nm || ld != nd
}

// dailyAtHour extracts the daily_at gate from task config. ok=false when
// the key is absent, the config is unparseable, or the hour is malformed
// (missing / outside 0-23). Malformed is treated as absent so a typo
// can't strand the task, and is WARN-logged once per task rather than on
// every tick.
func (s *Scheduler) dailyAtHour(task core.AgentTask) (hour int, ok bool) {
	if len(task.Config) == 0 {
		return 0, false
	}
	var cfg struct {
		DailyAt *struct {
			Hour *int `json:"hour"`
		} `json:"daily_at"`
	}
	if json.Unmarshal(task.Config, &cfg) != nil || cfg.DailyAt == nil {
		return 0, false
	}
	h := cfg.DailyAt.Hour
	if h == nil || *h < 0 || *h > 23 {
		s.warnBadDailyAt(task, h)
		return 0, false
	}
	return *h, true
}

// warnBadDailyAt logs a malformed daily_at hour a single time per task
// per process (see the dailyAtWarned field comment).
func (s *Scheduler) warnBadDailyAt(task core.AgentTask, hour *int) {
	id := task.ID.String()
	s.mu.Lock()
	if s.dailyAtWarned == nil {
		s.dailyAtWarned = make(map[string]bool)
	}
	seen := s.dailyAtWarned[id]
	s.dailyAtWarned[id] = true
	s.mu.Unlock()
	if seen {
		return
	}
	var hv any = "absent"
	if hour != nil {
		hv = *hour
	}
	s.logger.Warn("agent-tasks: invalid daily_at hour (want 0-23), gate ignored",
		"task_id", task.ID, "hour", hv)
}

func (s *Scheduler) isBusy(handler string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[handler]
}

func (s *Scheduler) setBusy(handler string, val bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy[handler] = val
}

func (s *Scheduler) trySetBusy(handler string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[handler] {
		return false
	}
	s.busy[handler] = true
	return true
}

// outputPreview is the short form of result.Output that lands on the
// "agent-tasks: completed" log line. 200 chars covers a typical Telegram-
// length reply; longer outputs get an ellipsis. The full text is in
// agent_tasks.result for anyone who needs the rest.
func outputPreview(s string) string {
	const maxRunes = 200
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// extractPeerTaskID pulls peer_task_id out of a Pause-progress payload
// for span annotation. Returns "" on any unmarshal error — span is
// best-effort, never fails the task.
func extractPeerTaskID(progress []byte) string {
	if len(progress) == 0 {
		return ""
	}
	var p struct {
		PeerTaskID string `json:"peer_task_id"`
	}
	_ = jsonUnmarshal(progress, &p)
	return p.PeerTaskID
}

func sessionIDFromProgress(progress []byte) string {
	if len(progress) == 0 {
		return ""
	}
	var p struct {
		SessionID string `json:"session_id"`
	}
	_ = jsonUnmarshal(progress, &p)
	return p.SessionID
}

func (s *Scheduler) archiveTaskSession(
	logCtx context.Context,
	taskID, soulID uuid.UUID,
	sessionID string,
) {
	if s.msgStore == nil || sessionID == "" {
		return
	}
	archiveCtx, archiveCancel := context.WithTimeout(
		core.WithSoulID(context.Background(), soulID),
		10*time.Second,
	)
	defer archiveCancel()
	if err := s.msgStore.ArchiveSession(archiveCtx, sessionID); err != nil {
		s.logger.WarnContext(logCtx, "agent-tasks: archive terminal session failed",
			"task_id", taskID, "session_id", sessionID, "error", err)
	}
}
