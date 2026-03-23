package agenttask

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/core"
)

// Scheduler polls agent_tasks and dispatches handlers.
type Scheduler struct {
	store    *core.AgentTaskStore
	handlers map[string]core.AgentHandler
	registry *core.ToolRegistry // master registry with all tools
	msgStore core.MessageStore  // session/message persistence
	deps     *core.Deps
	notify   func(ctx context.Context, userID uuid.UUID, text string)
	logger   *slog.Logger

	mu   sync.Mutex
	busy map[string]bool // handler name → currently executing
}

// NewScheduler creates an agent task scheduler.
func NewScheduler(
	store *core.AgentTaskStore,
	handlers map[string]core.AgentHandler,
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

// Run executes one scheduler tick: picks up pending tasks and dispatches handlers.
// Called by scheduler.RunLoop every 60 seconds.
func (s *Scheduler) Run(ctx context.Context) error {
	// Crash recovery: reset tasks stuck in 'running' for > 10 min.
	if n, err := s.store.ResetStale(ctx, 10*time.Minute); err != nil {
		s.logger.Warn("agent-tasks: reset stale failed", "error", err)
	} else if n > 0 {
		s.logger.Info("agent-tasks: reset stale tasks", "count", n)
	}

	tasks, err := s.store.PendingTasks(ctx)
	if err != nil {
		return err
	}

	for _, task := range tasks {
		handler, ok := s.handlers[task.Handler]
		if !ok {
			s.logger.Warn("agent-tasks: unknown handler", "handler", task.Handler, "task_id", task.ID)
			if err := s.store.Fail(ctx, task.ID, "unknown handler: "+task.Handler); err != nil {
				s.logger.Error("agent-tasks: fail update error", "error", err)
			}
			continue
		}

		if s.isBusy(task.Handler) {
			continue
		}

		// Check cron schedule for recurring tasks.
		if task.Schedule != nil && !s.shouldRunNow(task) {
			continue
		}

		go s.executeTask(ctx, task, handler)
	}

	return nil
}

func (s *Scheduler) executeTask(ctx context.Context, task core.AgentTask, handler core.AgentHandler) {
	s.setBusy(task.Handler, true)
	defer s.setBusy(task.Handler, false)

	s.logger.Info("agent-tasks: starting",
		"task_id", task.ID,
		"handler", task.Handler,
		"title", task.Title,
		"iteration", task.Iteration+1,
	)

	if err := s.store.SetRunning(ctx, task.ID); err != nil {
		s.logger.Error("agent-tasks: set running failed", "error", err)
		return
	}

	// Build scoped tool registry.
	var registry *core.ToolRegistry
	if len(task.Tools) > 0 {
		registry = s.registry.SubsetForNames(task.Tools)
	} else if tools := handler.DefaultTools(); len(tools) > 0 {
		registry = s.registry.SubsetForNames(tools)
	} else {
		registry = s.registry
	}

	agentDeps := core.AgentDeps{
		LLM:       s.deps.LLM,
		Registry:  registry,
		RoleTools: s.deps.RoleTools,
		Store:     s.msgStore,
		Prompts:   s.deps.Prompts,
		Logger:    s.logger,
		DB:        s.deps.DB,
		UserID:    task.UserID,
		Config:    s.deps.Config,
	}

	// Apply deadline as context timeout.
	execCtx := ctx
	if task.Deadline != nil && task.Deadline.After(time.Now()) {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithDeadline(ctx, *task.Deadline)
		defer cancel()
	}

	result, err := handler.Run(execCtx, task, agentDeps)
	if err != nil {
		s.logger.Error("agent-tasks: handler failed",
			"task_id", task.ID,
			"handler", task.Handler,
			"error", err,
		)
		if fErr := s.store.Fail(ctx, task.ID, err.Error()); fErr != nil {
			s.logger.Error("agent-tasks: fail update error", "error", fErr)
		}
		if s.notify != nil {
			s.notify(ctx, task.UserID, "Task failed: "+task.Title+"\n"+err.Error())
		}
		return
	}

	if result.Notify != "" && s.notify != nil {
		s.notify(ctx, task.UserID, result.Notify)
	}

	if result.Done {
		s.logger.Info("agent-tasks: completed",
			"task_id", task.ID,
			"handler", task.Handler,
		)
		if err := s.store.Complete(ctx, task.ID, result.Output); err != nil {
			s.logger.Error("agent-tasks: complete update error", "error", err)
		}
		// Recurring tasks: reset for next run.
		if task.Schedule != nil {
			if err := s.store.ResetForNextRun(ctx, task.ID); err != nil {
				s.logger.Error("agent-tasks: reset for next run error", "error", err)
			}
		}
		if result.Output != "" && s.notify != nil {
			s.notify(ctx, task.UserID, "Task done: "+task.Title+"\n\n"+result.Output)
		}
	} else {
		s.logger.Info("agent-tasks: iteration done",
			"task_id", task.ID,
			"handler", task.Handler,
			"iteration", task.Iteration+1,
		)
		if err := s.store.UpdateProgress(ctx, task.ID, result.Progress); err != nil {
			s.logger.Error("agent-tasks: progress update error", "error", err)
		}
	}
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
