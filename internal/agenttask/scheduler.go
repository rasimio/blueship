package agenttask

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/telemetry"
)

// jsonUnmarshal aliases stdlib json.Unmarshal so extractPeerTaskID can
// stay a one-liner without importing encoding/json at the call site.
var jsonUnmarshal = json.Unmarshal

// DefaultTaskTimeout is applied to tasks without an explicit deadline.
const DefaultTaskTimeout = 5 * time.Minute

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
	registryBuilder  func(userDeps *core.Deps) *core.ToolRegistry
	msgStore         core.MessageStore
	deps             *core.Deps
	notify           func(ctx context.Context, userID uuid.UUID, text string)
	onStatusChange   func(ctx context.Context, task core.AgentTask)
	logger           *slog.Logger

	mu     sync.Mutex
	busy   map[string]bool // task ID → currently executing
	taskWg sync.WaitGroup  // tracks in-flight executeTask goroutines
}

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
	notify func(ctx context.Context, userID uuid.UUID, text string),
	logger *slog.Logger,
) *Scheduler {
	return &Scheduler{
		store:            store,
		handlers:         handlers,
		strategyHandlers: strategyHandlers,
		registry:         registry,
		msgStore:         msgStore,
		deps:             deps,
		notify:           notify,
		logger:           logger,
		busy:             make(map[string]bool),
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

	// Auto-complete tasks that exhausted iterations but weren't marked done.
	s.store.CompleteExhausted(ctx)

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

		// Cadence guard for non-recurring tasks (e.g. periodic monitors
		// running on strategy=direct). Skips the tick without burning
		// an iteration if the task ran more recently than its cadence.
		if !s.cadenceElapsed(task) {
			continue
		}

		s.taskWg.Add(1)
		go s.executeTask(ctx, task, handler, dispatchTag)
	}

	return nil
}

// Wait blocks until all in-flight task goroutines complete.
// Called during graceful shutdown to ensure DB ops finish before connections close.
func (s *Scheduler) Wait() {
	s.taskWg.Wait()
}

func (s *Scheduler) executeTask(ctx context.Context, task core.AgentTask, handler core.AgentHandler, dispatchTag string) {
	defer s.taskWg.Done()
	s.setBusy(task.ID.String(), true)
	defer s.setBusy(task.ID.String(), false)

	// Tenant-attribute every write this iteration does. The task row
	// carries its own soul_id (denormalised in Phase A); thread it
	// through ctx so the handler, its tools, and the per-call DB ctxes
	// below all resolve the right soul.
	ctx = core.WithSoulID(ctx, task.SoulID)

	ctx, span := telemetry.StartTaskSpan(ctx, task.ID.String(), task.Handler, task.Strategy, dispatchTag, task.Iteration+1)
	defer span.End()

	s.logger.InfoContext(ctx, "agent-tasks: starting",
		"task_id", task.ID,
		"dispatch", dispatchTag,
		"title", task.Title,
		"iteration", task.Iteration+1,
	)

	if err := s.store.SetRunning(ctx, task.ID); err != nil {
		span.SetAttributes(attribute.String("agent_task.outcome", "set_running_failed"))
		telemetry.RecordError(span, err)
		s.logger.ErrorContext(ctx, "agent-tasks: set running failed", "task_id", task.ID, "error", err)
		return
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
	var registry *core.ToolRegistry
	if len(task.Tools) > 0 {
		registry = baseRegistry.SubsetForNames(task.Tools)
	} else if tools := handler.DefaultTools(); len(tools) > 0 {
		registry = baseRegistry.SubsetForNames(tools)
	} else {
		registry = baseRegistry
	}

	agentDeps := core.AgentDeps{
		LLM:             s.deps.LLM,
		Embedder:        s.deps.Embedder,
		Registry:        registry,
		RoleTools:       s.deps.RoleTools,
		ModelStore:      s.deps.ModelStore,
		Store:           s.msgStore,
		Prompts:         s.deps.Prompts,
		Users:           s.deps.Users,
		Sessions:        s.deps.Sessions,
		Logger:          s.logger,
		DB:              s.deps.DB,
		UserID:          task.UserID,
		Config:          s.deps.Config,
		SelfAgentID:     s.deps.SelfAgentID,
		ContextInjector: s.deps.ContextInjector,
		ReflexPreparer:  s.deps.ReflexPreparer,
		RuleEngine:      s.deps.RuleEngine,
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

	result, err := handler.Run(ctx, task, agentDeps)

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
		s.logger.ErrorContext(ctx, "agent-tasks: handler failed",
			"task_id", task.ID,
			"handler", task.Handler,
			"error", err,
		)
		dbCtx, dbCancel := newDBCtx()
		defer dbCancel()
		if fErr := s.store.SetPending(dbCtx, task.ID); fErr != nil {
			s.logger.ErrorContext(ctx, "agent-tasks: reset after fail error", "error", fErr)
		}
		return
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
	// shouldNotify computes the predicate once; each branch decides
	// whether to actually push to the user. Critical for research-style
	// agent_tasks with strict acceptance criteria: handler returns Done
	// with a long Output, evaluator rejects (0 URLs, etc.), and we used
	// to push the rejected draft to chat anyway because notify ran above
	// the gate. On 2026-05-10 task 988183c5 leaked a 6.5K-char fake
	// "AWM final report" to Telegram on iter 15 right before the gate
	// failed it for missing citations — exactly this bug.
	shouldNotify := result.Notify != "" && s.notify != nil && !strings.Contains(result.Notify, "[no-op]")
	notified := false

	if result.Pause {
		iterationOutcome = "pause"
		// Pause carries explicit milestone notifications (handler sets
		// Notify only when there's something user-actionable). Push.
		if shouldNotify {
			notifyCtx, notifyCancel := newDBCtx()
			s.notify(notifyCtx, task.UserID, result.Notify)
			notifyCancel()
			notified = true
		}
		span.SetAttributes(
			attribute.String("agent_task.outcome", "paused"),
			attribute.Bool("agent_task.notified", notified),
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
		return
	}

	if result.Done {
		// Acceptance criteria gate: if the task carries criteria and the
		// handler claims done on a non-recurring strategy, ask the LLM
		// to verify. Recurring jobs (Schedule != nil) always complete on
		// the handler's word.
		if task.Schedule == nil && task.AcceptanceCriteria != nil && *task.AcceptanceCriteria != "" {
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
				progressWithReason := injectFeedback(result.Progress, verdict.Reason)
				dbCtx, dbCancel := newDBCtx()
				defer dbCancel()
				if err := s.store.UpdateProgressWithRecheck(dbCtx, task.ID, progressWithReason, recheckURLs); err != nil {
					s.logger.ErrorContext(ctx, "agent-tasks: progress update error", "error", err)
				}
				return
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
		if shouldNotify {
			notifyCtx, notifyCancel := newDBCtx()
			s.notify(notifyCtx, task.UserID, result.Notify)
			notifyCancel()
			notified = true
		}
		span.SetAttributes(
			attribute.String("agent_task.outcome", "done"),
			attribute.Int("agent_task.output_size_bytes", len(result.Output)),
			attribute.Bool("agent_task.notified", notified),
		)
		if task.AcceptanceCriteria != nil && *task.AcceptanceCriteria != "" {
			span.SetAttributes(attribute.Bool("agent_task.acceptance_met", true))
		}
		s.logger.InfoContext(ctx, "agent-tasks: completed",
			"task_id", task.ID,
			"dispatch", dispatchTag,
			"output_size_bytes", len(result.Output),
			"output_preview", outputPreview(result.Output),
			"notified", notified,
		)
		completeCtx, completeCancel := newDBCtx()
		if err := s.store.Complete(completeCtx, task.ID, result.Output); err != nil {
			s.logger.ErrorContext(ctx, "agent-tasks: complete update error", "error", err)
		}
		completeCancel()
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
	} else {
		// Mid-task iteration. Push only when the handler explicitly
		// flagged something user-relevant via Notify (milestone, blocker)
		// — random in-progress output is noise, not a message.
		if shouldNotify {
			notifyCtx, notifyCancel := newDBCtx()
			s.notify(notifyCtx, task.UserID, result.Notify)
			notifyCancel()
			notified = true
		}
		span.SetAttributes(
			attribute.String("agent_task.outcome", "iteration_done"),
			attribute.Bool("agent_task.notified", notified),
		)
		s.logger.InfoContext(ctx, "agent-tasks: iteration done",
			"task_id", task.ID,
			"handler", task.Handler,
			"iteration", task.Iteration+1,
			"notified", notified,
		)
		dbCtx, dbCancel := newDBCtx()
		defer dbCancel()
		if err := s.store.UpdateProgress(dbCtx, task.ID, result.Progress); err != nil {
			s.logger.ErrorContext(ctx, "agent-tasks: progress update error", "error", err)
		}
	}
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
	return time.Since(*task.LastRunAt) >= d
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
	return time.Since(*task.LastRunAt) >= d
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
