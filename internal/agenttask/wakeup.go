package agenttask

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/rasimio/blueship/internal/core"
)

// ListenQueue wakes a local dispatcher on committed task changes from any
// process, including the API. The payload is only a schema, never task content.
// Reconnects cause a scan; PostgreSQL notifications are never the source of truth.
func ListenQueue(ctx context.Context, dsn, schema string, wake func(), logger *slog.Logger) {
	listener := pq.NewListener(dsn, time.Second, 30*time.Second, func(event pq.ListenerEventType, err error) {
		if err != nil {
			logger.Warn("agent-tasks: queue listener unavailable; polling remains active", "error", err)
		}
	})
	defer listener.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-done:
		}
	}()
	if err := listener.Listen("blueship_agent_task_queue"); err != nil {
		if ctx.Err() == nil {
			logger.Warn("agent-tasks: queue listen failed; polling remains active", "error", err)
		}
		return
	}
	wake() // Covers commits between the scheduler's initial scan and LISTEN.
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-listener.Notify:
			if !ok {
				return
			}
			if n == nil || n.Extra == schema {
				wake()
			}
		case <-ping.C:
			if err := listener.Ping(); err != nil && ctx.Err() == nil {
				logger.Warn("agent-tasks: queue listener ping failed", "error", err)
			}
		}
	}
}

// SetWakeup installs a coalesced local hint for newly available worker capacity.
func (s *Scheduler) SetWakeup(wake func()) { s.wakeup = wake }
func (s *Scheduler) signalWakeup() {
	if s.wakeup != nil {
		s.wakeup()
	}
}

// scheduleWakeupAt retains only the earliest hint. Periodic polling covers long
// waits; a bounded timer handles near-term retry and lease boundaries precisely.
func (s *Scheduler) scheduleWakeupAt(ctx context.Context, at time.Time) {
	if s.wakeup == nil || at.IsZero() || ctx.Err() != nil {
		return
	}
	delay := time.Until(at)
	if delay > time.Minute {
		return
	}
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.wakeTimer != nil && !at.Before(s.wakeAt) {
		return
	}
	if s.wakeTimer != nil {
		s.wakeTimer.Stop()
	}
	s.wakeAt = at
	s.wakeTimer = time.AfterFunc(max(delay, 25*time.Millisecond), func() {
		s.wakeMu.Lock()
		current := s.wakeAt.Equal(at)
		if current {
			s.wakeTimer = nil
			s.wakeAt = time.Time{}
		}
		s.wakeMu.Unlock()
		if current && ctx.Err() == nil {
			s.signalWakeup()
		}
	})
}

func (s *Scheduler) schedulePersistedWake(ctx context.Context) {
	if s.wakeup == nil || s.store == nil || ctx.Err() != nil {
		return
	}
	queryCtx, end := context.WithTimeout(ctx, 2*time.Second)
	defer end()
	at, err := s.store.NextGraphWake(queryCtx)
	if err != nil {
		s.logger.WarnContext(ctx, "agent-tasks: next wake unavailable; polling remains active", "error", err)
		at = time.Time{}
	}
	if hostAt := s.nextRecurringWake(queryCtx, time.Now()); !hostAt.IsZero() && (at.IsZero() || hostAt.Before(at)) {
		at = hostAt
	}
	s.scheduleWakeupAt(ctx, at)
}

// nextRecurringWake is the host's next due moment for a recurring task. A
// past moment is dropped: armed as a timer it would fire at the 25ms floor,
// and the tick it starts would arm it again.
func (s *Scheduler) nextRecurringWake(ctx context.Context, now time.Time) time.Time {
	if s.deps == nil || s.deps.RecurringTaskWake == nil {
		return time.Time{}
	}
	at, err := s.deps.RecurringTaskWake.NextWake(ctx, now)
	if err != nil {
		s.logger.WarnContext(ctx, "agent-tasks: recurring wake unavailable; schedules still apply", "error", err)
		return time.Time{}
	}
	if !at.After(now) {
		return time.Time{}
	}
	return at
}

// recurringDueEarly asks the host which recurring tasks, not yet due by
// their schedule, have work that came due since their last run. A failure
// only delays that work to the next scheduled run.
func (s *Scheduler) recurringDueEarly(ctx context.Context, tasks []core.AgentTask, now time.Time) map[uuid.UUID]bool {
	if s.deps == nil || s.deps.RecurringTaskWake == nil {
		return nil
	}
	var waiting []core.AgentTask
	for _, task := range tasks {
		if task.Schedule == nil || task.LastRunAt == nil {
			continue
		}
		// shouldRunNow without its warning: the dispatch loop logs a bad
		// schedule once per tick, and such a task is not brought forward.
		d, err := time.ParseDuration(*task.Schedule)
		if err == nil && !durationDue(now.Sub(*task.LastRunAt), d) {
			waiting = append(waiting, task)
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	queryCtx, end := context.WithTimeout(ctx, 2*time.Second)
	defer end()
	due, err := s.deps.RecurringTaskWake.DueEarly(queryCtx, now, waiting)
	if err != nil {
		s.logger.WarnContext(ctx, "agent-tasks: early-run check failed; schedules still apply", "error", err)
		return nil
	}
	return due
}

// StopWakeups releases the timer after the scheduler loop has stopped.
func (s *Scheduler) StopWakeups() {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.wakeTimer != nil {
		s.wakeTimer.Stop()
		s.wakeTimer = nil
	}
	s.wakeAt = time.Time{}
}
