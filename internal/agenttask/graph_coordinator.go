package agenttask

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

// GraphTaskRunner owns planning, accepted step execution and finalization.
// Before returning successfully it must either finalize or yield the task.
// The scheduler supplies tenant identity and a live, continuously renewed run.
type GraphTaskRunner func(context.Context, core.AgentTask) error

type GraphCoordinator struct {
	Store *core.AgentTaskStore
	Lease time.Duration
}

func (c GraphCoordinator) Run(ctx context.Context, id uuid.UUID, work GraphTaskRunner) (bool, error) {
	if c.Store == nil || work == nil || c.Lease < time.Second || c.Lease > 10*time.Minute {
		return false, fmt.Errorf("invalid graph coordinator configuration")
	}
	run, err := c.Store.ClaimGraphTask(ctx, id, c.Lease)
	if err != nil || run == nil {
		return false, err
	}
	task, err := c.Store.Get(ctx, id)
	if err != nil {
		return true, err
	}
	ctx = core.WithTaskRunID(core.WithUserID(core.WithSoulID(ctx, task.SoulID), task.UserID), run.RunID)
	var cancel context.CancelFunc
	if deadline := core.TaskStopDeadline(task); deadline != nil {
		ctx, cancel = context.WithDeadline(ctx, *deadline)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	stop := make(chan struct{})
	stopped := make(chan error, 1)
	go func() {
		tick := time.NewTicker(c.Lease / 3)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				stopped <- nil
				return
			case <-ctx.Done():
				stopped <- ctx.Err()
				return
			case <-tick.C:
				renewCtx, end := context.WithTimeout(ctx, c.Lease/3)
				err := c.Store.RenewGraphTask(renewCtx, id, c.Lease)
				end()
				if err != nil {
					cancel()
					stopped <- err
					return
				}
			}
		}
	}()
	err = work(ctx, task)
	close(stop)
	renewalErr := <-stopped
	// Finalizing/yielding intentionally invalidates the lease. A concurrent
	// renewal may observe that successful transition before work returns.
	checkCtx, end := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer end()
	current, readErr := c.Store.Get(checkCtx, id)
	if readErr != nil {
		return true, errors.Join(err, renewalErr, readErr)
	}
	transitioned := current.Status == "done" || current.Status == "failed" || current.Status == "canceled" || current.Status == "pending" || current.Status == "paused"
	if transitioned {
		return true, err
	}
	if err == nil && renewalErr == nil {
		err = fmt.Errorf("graph runner returned without yielding or finalizing")
	}
	return true, errors.Join(err, renewalErr)
}

// SetGraphRunner installs v2 processing without changing how existing v1 tasks
// run. Creation remains separately gated until the host adapter is installed.
func (s *Scheduler) SetGraphRunner(runner GraphTaskRunner) { s.graphRunner = runner }

func (s *Scheduler) dispatchGraphTasks(ctx context.Context) error {
	if s.graphRunner == nil {
		return nil
	}
	tasks, err := s.store.RunnableGraphTasks(ctx, maxConcurrentTasks*2)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if !s.executionAllowed(ctx, task) || !s.startAtGateOpen(task, time.Now()) {
			continue
		}
		if !s.trySetBusy(task.ID.String()) {
			continue
		}
		select {
		case s.sem <- struct{}{}:
		default:
			s.setBusy(task.ID.String(), false)
			return nil
		}
		s.taskWg.Add(1)
		go func(task core.AgentTask) {
			defer s.taskWg.Done()
			defer func() { <-s.sem; s.setBusy(task.ID.String(), false); s.signalWakeup() }()
			coordinator := GraphCoordinator{Store: s.store, Lease: 30 * time.Second}
			_, err := coordinator.Run(ctx, task.ID, s.graphRunner)
			if err != nil {
				s.logger.ErrorContext(ctx, "agent-tasks: graph execution stopped", "task_id", task.ID, "error", err)
			}
		}(task)
	}
	return nil
}

func (s *Scheduler) maintainGraphDeadlines(ctx context.Context, now time.Time) error {
	tasks, err := s.store.ExpiredGraphTasks(ctx, now, 100)
	if err != nil {
		return err
	}
	changed := false
	for _, task := range tasks {
		expired, err := s.expireTask(ctx, task, now, s.store)
		if err != nil {
			return err
		}
		changed = changed || expired
	}
	if changed {
		s.drainNotificationsAsync(ctx)
	}
	return nil
}
