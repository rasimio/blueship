package agenttask

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

type fakeRecurringWake struct {
	next    time.Time
	nextErr error
	due     map[uuid.UUID]bool
	dueErr  error
	asked   []core.AgentTask
}

func (f *fakeRecurringWake) NextWake(context.Context, time.Time) (time.Time, error) {
	return f.next, f.nextErr
}

func (f *fakeRecurringWake) DueEarly(_ context.Context, _ time.Time, tasks []core.AgentTask) (map[uuid.UUID]bool, error) {
	f.asked = append(f.asked, tasks...)
	return f.due, f.dueErr
}

// A heartbeat that ran at 19:39 under a 5-minute schedule is the only task
// worth asking about at 19:42: the others are already due, have never run,
// or have no schedule to bring forward.
func TestRecurringDueEarlyAsksOnlyAboutWaitingTasks(t *testing.T) {
	now := time.Date(2026, 10, 14, 19, 42, 0, 0, time.UTC)
	five, bad := "5m", "every five minutes"
	ranRecently, ranLongAgo := now.Add(-3*time.Minute), now.Add(-6*time.Minute)
	waiting := core.AgentTask{ID: uuid.New(), Schedule: &five, LastRunAt: &ranRecently}
	tasks := []core.AgentTask{
		waiting,
		{ID: uuid.New(), Schedule: &five, LastRunAt: &ranLongAgo},
		{ID: uuid.New(), Schedule: &five},
		{ID: uuid.New(), LastRunAt: &ranRecently},
		{ID: uuid.New(), Schedule: &bad, LastRunAt: &ranRecently},
	}
	wake := &fakeRecurringWake{due: map[uuid.UUID]bool{waiting.ID: true}}
	s := &Scheduler{logger: slog.New(slog.DiscardHandler), deps: &core.Deps{RecurringTaskWake: wake}}

	early := s.recurringDueEarly(context.Background(), tasks, now)
	if len(wake.asked) != 1 || wake.asked[0].ID != waiting.ID {
		t.Fatalf("host was asked about %d tasks, want only the waiting heartbeat: %+v", len(wake.asked), wake.asked)
	}
	if !early[waiting.ID] {
		t.Fatalf("the host's answer was dropped: %v", early)
	}
}

func TestRecurringDueEarlyFallsBackToSchedules(t *testing.T) {
	now := time.Now()
	five := "5m"
	ran := now.Add(-time.Minute)
	tasks := []core.AgentTask{{ID: uuid.New(), Schedule: &five, LastRunAt: &ran}}

	none := &Scheduler{logger: slog.New(slog.DiscardHandler), deps: &core.Deps{}}
	if got := none.recurringDueEarly(context.Background(), tasks, now); got != nil {
		t.Fatalf("no host hook, yet tasks were brought forward: %v", got)
	}
	failing := &fakeRecurringWake{due: map[uuid.UUID]bool{tasks[0].ID: true}, dueErr: errors.New("db down")}
	s := &Scheduler{logger: slog.New(slog.DiscardHandler), deps: &core.Deps{RecurringTaskWake: failing}}
	if got := s.recurringDueEarly(context.Background(), tasks, now); got != nil {
		t.Fatalf("a failed check brought tasks forward: %v", got)
	}
}

// A past moment armed as a timer fires at the 25ms floor and the tick it
// starts arms it again, so only a future moment may reach the timer.
func TestNextRecurringWakeKeepsOnlyFutureMoments(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		wake *fakeRecurringWake
		want time.Time
	}{
		"future": {&fakeRecurringWake{next: now.Add(30 * time.Second)}, now.Add(30 * time.Second)},
		"now":    {&fakeRecurringWake{next: now}, time.Time{}},
		"past":   {&fakeRecurringWake{next: now.Add(-time.Second)}, time.Time{}},
		"none":   {&fakeRecurringWake{}, time.Time{}},
		"error":  {&fakeRecurringWake{next: now.Add(time.Second), nextErr: errors.New("db down")}, time.Time{}},
	} {
		s := &Scheduler{logger: slog.New(slog.DiscardHandler), deps: &core.Deps{RecurringTaskWake: tc.wake}}
		if got := s.nextRecurringWake(context.Background(), now); !got.Equal(tc.want) {
			t.Errorf("%s: next wake %v, want %v", name, got, tc.want)
		}
	}
}
