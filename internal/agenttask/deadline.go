package agenttask

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

const taskProgressAfter = 10 * time.Minute

type taskDeadlineStore interface {
	DeadlineTasks(context.Context) ([]core.AgentTask, error)
	SetTaskDeadline(context.Context, uuid.UUID, time.Time) error
	ExpireTask(context.Context, uuid.UUID, time.Time) (bool, error)
}

func (s *Scheduler) taskWallTimeout() time.Duration {
	if s.deps != nil && s.deps.Config != nil && s.deps.Config.Timeouts.TaskWall > 0 {
		return s.deps.Config.Timeouts.TaskWall
	}
	return core.DefaultTaskWallTimeout
}

// Maintenance runs independently of dispatch, so admission denial, a full
// worker pool, pauses or a restart cannot make a task wait indefinitely.
func (s *Scheduler) maintainTaskDeadlines(ctx context.Context, now time.Time, store taskDeadlineStore) error {
	tasks, err := store.DeadlineTasks(ctx)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.Status == "failed" {
			s.notifyTaskLifecycle(ctx, task, "deadline")
			continue
		}
		deadline := core.TaskWallDeadline(task, now, s.taskWallTimeout())
		if deadline == nil {
			continue
		}
		if task.Deadline == nil || !task.Deadline.Equal(*deadline) {
			if err := store.SetTaskDeadline(ctx, task.ID, *deadline); err != nil {
				return err
			}
			task.Deadline = deadline
		}
		if !deadline.After(now) {
			if _, err := s.expireTask(ctx, task, now, store); err != nil {
				return err
			}
			continue
		}
		// A single checkpoint covers both queued and executing work. Explicit
		// delayed starts and periodic jobs must not receive premature progress.
		if task.Schedule == nil && task.Cadence == nil && task.Strategy != core.StrategyRecurring &&
			!core.TaskWallStart(task, now).Add(taskProgressAfter).After(now) {
			s.notifyTaskLifecycle(ctx, task, "progress")
		}
	}
	return nil
}

func (s *Scheduler) expireTask(ctx context.Context, task core.AgentTask, now time.Time, store taskDeadlineStore) (bool, error) {
	// The worker normally reaches this branch because its context expired.
	// Persist that terminal state on a fresh, bounded DB/notification context.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notificationAttemptTimeout)
	defer cancel()
	changed, err := store.ExpireTask(ctx, task.ID, now)
	if err != nil || !changed {
		return changed, err
	}
	task.Status = "failed"
	reason := core.TaskDeadlineExceeded
	task.ErrorMessage, task.CompletedAt = &reason, &now
	s.notifyTaskLifecycle(ctx, task, "deadline")
	if s.store != nil {
		sessionID := sessionIDFromProgress(task.Progress)
		if task.SessionID != nil {
			sessionID = *task.SessionID
		}
		s.archiveTaskSession(ctx, task.ID, task.SoulID, sessionID)
	}
	if s.onStatusChange != nil {
		go s.onStatusChange(context.WithoutCancel(ctx), task)
	}
	return true, nil
}

func (s *Scheduler) notifyTaskLifecycle(ctx context.Context, task core.AgentTask, stage string) {
	cfg := core.Config{}
	if s.deps != nil && s.deps.Config != nil {
		cfg = *s.deps.Config
	}
	cfg.ApplyDefaults()
	format := cfg.UI.TaskProgressQueuedFmt
	if task.LastRunAt != nil || task.Iteration > 0 {
		format = cfg.UI.TaskProgressRunningFmt
	}
	if stage == "deadline" {
		format = cfg.UI.TaskDeadlineFmt
	}
	notifyCtx := core.WithUserID(core.WithSoulID(ctx, task.SoulID), task.UserID)
	refs := []core.TaskDeliveryRef{{InputID: "task_lifecycle", ItemKey: stage}}
	_, err := deliverTaskNotification(notifyCtx, s.validatedTaskNotifier(task, refs), s.notifyJournal, task.ID, task.UserID,
		fmt.Sprintf(format, task.Title), refs)
	if err != nil {
		s.logger.WarnContext(ctx, "agent-tasks: lifecycle notification failed", "task_id", task.ID, "stage", stage, "error", err)
	}
}

func iterationDeadline(now time.Time, task core.AgentTask) time.Time {
	deadline := now.Add(DefaultTaskTimeout)
	if task.Deadline != nil && task.Deadline.Before(deadline) {
		deadline = *task.Deadline
	}
	return deadline
}
