// Package goal holds the long-running-task primitive for BlueShip: the
// Goal scheduler + the strategy-specific handlers. Goals are distinct
// from agent_tasks (recurring scheduled jobs) in lifecycle, state
// machine, and entity shape.
package goal

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/core"
)

// DefaultGoalTimeout caps a single iteration's runtime when the goal
// itself doesn't have a deadline. Picked to be well above typical
// plan-stage LLM time but below indefinite hang.
const DefaultGoalTimeout = 10 * time.Minute

// Scheduler polls the goals table and dispatches strategy handlers.
// Mirror of agenttask.Scheduler structurally; the two run side-by-side
// because goals and scheduled tasks have fundamentally different
// lifecycles.
type Scheduler struct {
	store    *core.GoalStore
	handlers map[core.GoalStrategy]core.GoalHandler
	registry *core.ToolRegistry
	msgStore core.MessageStore
	deps     *core.Deps
	notify   func(ctx context.Context, userID uuid.UUID, text string)
	logger   *slog.Logger

	mu     sync.Mutex
	busy   map[string]bool
	goalWg sync.WaitGroup
}

// NewScheduler constructs a goal scheduler. handlers maps each supported
// GoalStrategy to its executor. A goal whose strategy isn't in this map
// fails on first dispatch with a descriptive error.
func NewScheduler(
	store *core.GoalStore,
	handlers map[core.GoalStrategy]core.GoalHandler,
	registry *core.ToolRegistry,
	msgStore core.MessageStore,
	deps *core.Deps,
	notify func(ctx context.Context, userID uuid.UUID, text string),
	logger *slog.Logger,
) *Scheduler {
	return &Scheduler{
		store:    store,
		handlers: handlers,
		registry: registry,
		msgStore: msgStore,
		deps:     deps,
		notify:   notify,
		logger:   logger,
		busy:     make(map[string]bool),
	}
}

// WakeFromCallback is called by the surrounding RunLoopWithTrigger when an
// A2A callback arrives for a peer task. Transitions the paused goal back
// to pending so the next scheduler tick picks it up.
func (s *Scheduler) WakeFromCallback(ctx context.Context, peerTaskID string) {
	if peerTaskID == "" {
		return
	}
	wokenID, err := s.store.WakePausedByPeerTask(ctx, peerTaskID)
	if err != nil {
		s.logger.Info("goals: no paused goal for callback", "peer_task", peerTaskID)
		return
	}
	s.logger.Info("goals: woke paused goal from callback",
		"goal_id", wokenID, "peer_task", peerTaskID)
}

// Run executes one scheduler tick: recovery + pending pickup.
func (s *Scheduler) Run(ctx context.Context) error {
	s.logger.Info("goals: tick")

	// Auto-fail goals that burned all iterations.
	if err := s.store.CompleteExhausted(ctx); err != nil {
		s.logger.Warn("goals: complete exhausted failed", "error", err)
	}

	// Crash recovery: reset goals stuck running > 10 min back to pending.
	if n, err := s.store.ResetStaleRunning(ctx, 10*time.Minute); err != nil {
		s.logger.Warn("goals: reset stale failed", "error", err)
	} else if n > 0 {
		s.logger.Info("goals: reset stale running", "count", n)
	}

	// Watchdog: wake paused goals that haven't received a callback in 30 min.
	if n, err := s.store.WakeStalePaused(ctx, 30*time.Minute); err != nil {
		s.logger.Warn("goals: wake stale paused failed", "error", err)
	} else if n > 0 {
		s.logger.Info("goals: woke stale paused (lost callback?)", "count", n)
	}

	goals, err := s.store.PendingGoals(ctx)
	if err != nil {
		s.logger.Error("goals: pending query failed", "error", err)
		return err
	}
	s.logger.Info("goals: pending", "count", len(goals))

	for _, g := range goals {
		handler, ok := s.handlers[g.Strategy]
		if !ok {
			msg := fmt.Sprintf("unsupported goal strategy: %s", g.Strategy)
			s.logger.Warn("goals: "+msg, "goal_id", g.ID)
			if err := s.store.Fail(ctx, g.ID, msg); err != nil {
				s.logger.Error("goals: fail update error", "error", err)
			}
			continue
		}

		if s.isBusy(g.ID.String()) {
			continue
		}

		s.goalWg.Add(1)
		go s.executeGoal(ctx, g, handler)
	}
	return nil
}

// Wait blocks until all in-flight goal goroutines complete. Called during
// graceful shutdown to ensure DB writes finish before connections close.
func (s *Scheduler) Wait() {
	s.goalWg.Wait()
}

func (s *Scheduler) executeGoal(ctx context.Context, g core.Goal, handler core.GoalHandler) {
	defer s.goalWg.Done()
	s.setBusy(g.ID.String(), true)
	defer s.setBusy(g.ID.String(), false)

	s.logger.Info("goals: starting",
		"goal_id", g.ID,
		"strategy", g.Strategy,
		"title", g.Title,
		"iteration", g.Iteration+1,
	)

	if err := s.store.SetRunning(ctx, g.ID); err != nil {
		s.logger.Error("goals: set running failed", "error", err)
		return
	}

	// Build scoped tool registry: if goal restricts tools, subset; else full.
	var registry *core.ToolRegistry
	if len(g.Tools) > 0 {
		registry = s.registry.SubsetForNames(g.Tools)
	} else {
		registry = s.registry
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
		UserID:          g.UserID,
		Config:          s.deps.Config,
		ContextInjector: s.deps.ContextInjector,
		ReflexPreparer:  s.deps.ReflexPreparer,
		RuleEngine:      s.deps.RuleEngine,
	}

	ctx, cancel := context.WithTimeout(ctx, DefaultGoalTimeout)
	defer cancel()

	result, err := handler.Run(ctx, g, agentDeps)

	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dbCancel()

	if err != nil {
		s.logger.Error("goals: handler failed",
			"goal_id", g.ID,
			"strategy", g.Strategy,
			"error", err,
		)
		if fErr := s.store.Fail(dbCtx, g.ID, err.Error()); fErr != nil {
			s.logger.Error("goals: fail update error", "error", fErr)
		}
		return
	}

	if result.Notify != "" && s.notify != nil && !strings.Contains(result.Notify, "[no-op]") {
		s.notify(dbCtx, g.UserID, result.Notify)
	}

	if result.Pause {
		s.logger.Info("goals: paused (waiting for callback)",
			"goal_id", g.ID,
			"strategy", g.Strategy,
			"iteration", g.Iteration+1,
		)
		if err := s.store.PauseGoal(dbCtx, g.ID, result.Progress); err != nil {
			s.logger.Error("goals: pause update error", "error", err)
		}
		return
	}

	if result.Done {
		s.logger.Info("goals: completed",
			"goal_id", g.ID,
			"strategy", g.Strategy,
		)
		if err := s.store.Complete(dbCtx, g.ID, result.Output); err != nil {
			s.logger.Error("goals: complete update error", "error", err)
		}
		return
	}

	s.logger.Info("goals: iteration done",
		"goal_id", g.ID,
		"strategy", g.Strategy,
		"iteration", g.Iteration+1,
	)
	if err := s.store.UpdateProgress(dbCtx, g.ID, result.Progress); err != nil {
		s.logger.Error("goals: progress update error", "error", err)
	}
}

func (s *Scheduler) isBusy(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[id]
}

func (s *Scheduler) setBusy(id string, val bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy[id] = val
}
