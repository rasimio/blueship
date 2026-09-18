package agenttask

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

type deadlineStoreFake struct{ tasks map[uuid.UUID]core.AgentTask }

func (f *deadlineStoreFake) DeadlineTasks(context.Context) ([]core.AgentTask, error) {
	var result []core.AgentTask
	for _, task := range f.tasks {
		result = append(result, task)
	}
	return result, nil
}
func (f *deadlineStoreFake) SetTaskDeadline(_ context.Context, id uuid.UUID, deadline time.Time) error {
	task := f.tasks[id]
	task.Deadline = &deadline
	f.tasks[id] = task
	return nil
}
func (f *deadlineStoreFake) ExpireTask(_ context.Context, id uuid.UUID, now time.Time) (bool, error) {
	task := f.tasks[id]
	if task.Status == "failed" || task.Deadline == nil || task.Deadline.After(now) {
		return false, nil
	}
	task.Status = "failed"
	task.CompletedAt = &now
	reason := core.TaskDeadlineExceeded
	task.ErrorMessage = &reason
	f.tasks[id] = task
	return true, nil
}

type deadlineContextStore struct{ deadlineStoreFake }

func (f *deadlineContextStore) ExpireTask(ctx context.Context, id uuid.UUID, now time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return f.deadlineStoreFake.ExpireTask(ctx, id, now)
}

func TestExpiryPersistsAfterWorkerContextDeadline(t *testing.T) {
	now := time.Now()
	deadline := now.Add(-time.Second)
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Status: "running", Deadline: &deadline}
	store := &deadlineContextStore{deadlineStoreFake{tasks: map[uuid.UUID]core.AgentTask{task.ID: task}}}
	s := &Scheduler{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), notifyJournal: &schedulerNotificationJournal{},
		notify: func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
			return core.TaskNotificationReceipt{}, nil
		},
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if changed, err := s.expireTask(ctx, task, now, store); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if store.tasks[task.ID].Status != "failed" {
		t.Fatal("expired worker left task running")
	}
}

func TestDeadlineMaintenanceExpiresQueuedPausedAndRunningAcrossRestart(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	store := &deadlineStoreFake{tasks: map[uuid.UUID]core.AgentTask{}}
	for _, status := range []string{"pending", "paused", "running"} {
		task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), SoulID: uuid.New(), Title: status,
			Strategy: core.StrategyDirect, Status: status, CreatedAt: now.Add(-21 * time.Hour)}
		store.tasks[task.ID] = task
	}
	seen := map[uuid.UUID]bool{}
	sent := 0
	journal := &schedulerNotificationJournal{begin: func(_ context.Context, id, _ uuid.UUID, _ string, refs []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
		if len(refs) != 1 || refs[0].InputID != "task_lifecycle" || refs[0].ItemKey != "deadline" {
			t.Fatal(refs)
		}
		fresh := !seen[id]
		seen[id] = true
		return uuid.New(), fresh, nil
	}}
	makeScheduler := func() *Scheduler {
		return &Scheduler{
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)), notifyJournal: journal,
			notify: func(ctx context.Context, id uuid.UUID, text string) (core.TaskNotificationReceipt, error) {
				if !strings.Contains(text, "time limit") {
					t.Fatal(text)
				}
				if core.UserIDFromContext(ctx) != id {
					t.Fatal("missing tenant identity")
				}
				sent++
				return core.TaskNotificationReceipt{}, nil
			},
		}
	}
	if err := makeScheduler().maintainTaskDeadlines(context.Background(), now, store); err != nil {
		t.Fatal(err)
	}
	if sent != 3 {
		t.Fatalf("sent %d, want 3", sent)
	}
	for _, task := range store.tasks {
		if task.Status != "failed" || task.CompletedAt == nil || task.ErrorMessage == nil || *task.ErrorMessage != core.TaskDeadlineExceeded {
			t.Fatal(task)
		}
	}
	// New scheduler, same persistent rows/journal: no new budget and no duplicate messages.
	if err := makeScheduler().maintainTaskDeadlines(context.Background(), now.Add(time.Hour), store); err != nil {
		t.Fatal(err)
	}
	if sent != 3 {
		t.Fatalf("duplicate status delivery after restart: %d", sent)
	}
}

func TestDeadlineNotificationRecoversAfterReservationFailure(t *testing.T) {
	now := time.Now()
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Strategy: core.StrategyDirect,
		Status: "pending", CreatedAt: now.Add(-time.Hour)}
	store := &deadlineStoreFake{tasks: map[uuid.UUID]core.AgentTask{task.ID: task}}
	fail := true
	sent := 0
	s := &Scheduler{logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		notifyJournal: &schedulerNotificationJournal{begin: func(context.Context, uuid.UUID, uuid.UUID, string, []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
			if fail {
				return uuid.Nil, false, errors.New("database temporarily unavailable")
			}
			return uuid.New(), true, nil
		}}, notify: func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
			sent++
			return core.TaskNotificationReceipt{}, nil
		},
	}
	if err := s.maintainTaskDeadlines(context.Background(), now, store); err != nil {
		t.Fatal(err)
	}
	if sent != 0 || store.tasks[task.ID].Status != "failed" {
		t.Fatal("expiration must not depend on notification success")
	}
	fail = false
	if err := s.maintainTaskDeadlines(context.Background(), now, store); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatal("terminal status notification was lost")
	}
}

func TestProgressCheckpointAndIterationDeadline(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Title: "research", Strategy: core.StrategyDirect,
		Status: "pending", CreatedAt: now}
	store := &deadlineStoreFake{tasks: map[uuid.UUID]core.AgentTask{task.ID: task}}
	seen := false
	sent := 0
	s := &Scheduler{logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		notifyJournal: &schedulerNotificationJournal{begin: func(_ context.Context, _, _ uuid.UUID, text string, refs []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
			if !strings.Contains(text, "waiting") || refs[0].ItemKey != "progress" {
				t.Fatal(text, refs)
			}
			fresh := !seen
			seen = true
			return uuid.New(), fresh, nil
		}}, notify: func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
			sent++
			return core.TaskNotificationReceipt{}, nil
		},
	}
	for _, offset := range []time.Duration{0, 9 * time.Minute, 10 * time.Minute, 11 * time.Minute} {
		if err := s.maintainTaskDeadlines(context.Background(), now.Add(offset), store); err != nil {
			t.Fatal(err)
		}
	}
	if sent != 1 {
		t.Fatalf("status sent %d times, want 1", sent)
	}
	deadline := now.Add(2 * time.Minute)
	task.Deadline = &deadline
	if got := iterationDeadline(now, task); !got.Equal(deadline) {
		t.Fatal("iteration exceeded remaining wall budget")
	}
	deadline = now.Add(-time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), iterationDeadline(now, task))
	defer cancel()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("expired task got a fresh iteration budget")
	}
}

func TestTerminalTaskDoesNotReceiveOldProgressRetry(t *testing.T) {
	called, rejected := false, false
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Status: "failed"}
	s := &Scheduler{notifyTask: func(context.Context, uuid.UUID) (core.AgentTask, error) { return task, nil },
		notify: func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
			called = true
			return core.TaskNotificationReceipt{}, nil
		},
		notifyJournal: &schedulerNotificationJournal{reject: func(context.Context, uuid.UUID, string) error { rejected = true; return nil }},
	}
	err := s.retryTaskNotification(context.Background(), core.TaskNotificationIntent{ID: uuid.New(), TaskID: task.ID, UserID: task.UserID,
		Text: "still running", Refs: []core.TaskDeliveryRef{{InputID: "task_lifecycle", ItemKey: "progress"}}})
	if called || !rejected || err == nil {
		t.Fatal("stale status was sent after terminal transition", called, rejected, err)
	}
}
