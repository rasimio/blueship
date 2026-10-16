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

// A reminder due at 13:59 waited behind a chat turn, was deferred for a retry
// the gateway asked to make in one second, and arrived at 14:01:09: the delay
// was floored at a minute and then waited for the next minute's poll
// (2026-10-15). A delay the sender names is kept, and the scheduler wakes for it.
func TestDeferredNotificationKeepsTheSendersDelay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		min, max time.Duration
	}{
		{name: "conversation busy", err: core.DefinitelyNotSentAfter(errors.New("inbound user activity is pending"), time.Second), min: 900 * time.Millisecond, max: 2 * time.Second},
		{name: "telegram retry_after", err: core.DefinitelyNotSentAfter(errors.New("telegram 429"), 5*time.Second), min: 4900 * time.Millisecond, max: 6 * time.Second},
		{name: "no delay named floors at a second", err: core.DefinitelyNotSentAfter(errors.New("busy"), 0), min: 900 * time.Millisecond, max: 2 * time.Second},
		{name: "unnamed delay keeps the default", err: core.DefinitelyNotSent(errors.New("lookup failed")), min: defaultNotificationRetryDelay - time.Second, max: defaultNotificationRetryDelay + time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var persisted time.Time
			journal := &schedulerNotificationJournal{deferAttempt: func(_ context.Context, _ uuid.UUID, _ string, retryAt time.Time) error {
				persisted = retryAt
				return nil
			}}
			started := time.Now()
			delivered, err := resolveTaskNotificationAttempt(context.Background(), journal, uuid.New(), core.TaskNotificationReceipt{}, tc.err)
			if delivered || !core.IsDefinitelyNotSent(err) {
				t.Fatalf("delivered=%v err=%v; want a definitely-not-sent deferral", delivered, err)
			}
			if wait := persisted.Sub(started); wait < tc.min || wait > tc.max {
				t.Fatalf("retry in %s, want %s..%s", wait, tc.min, tc.max)
			}
			var deferred *notificationDeferredError
			if !errors.As(err, &deferred) || !deferred.retryAt.Equal(persisted) {
				t.Fatalf("error %v does not carry the persisted retry moment %s", err, persisted)
			}
		})
	}
}

func TestDeferredNotificationWakesTheScheduler(t *testing.T) {
	events := make(chan struct{}, 2)
	s := &Scheduler{wakeup: func() { events <- struct{}{} }}
	defer s.StopWakeups()
	s.wakeForNotificationRetry(context.Background(), errors.New("transport down"))
	s.wakeForNotificationRetry(context.Background(), &notificationDeferredError{err: errors.New("busy"), retryAt: time.Now().Add(40 * time.Millisecond)})
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("a deferred notification waited for the poll")
	}
	select {
	case <-events:
		t.Fatal("an error without a retry moment armed a wake")
	case <-time.After(80 * time.Millisecond):
	}
}

type retryClockJournal struct {
	schedulerNotificationJournal
	next time.Time
	err  error
}

func (j *retryClockJournal) NextNotificationRetry(context.Context, time.Time) (time.Time, error) {
	return j.next, j.err
}

// Every run re-arms the wake for the next deferred notification, so a retry
// deferred while an earlier wake was armed is not left to the poll either.
func TestPersistedWakeIncludesDeferredNotifications(t *testing.T) {
	now := time.Now()
	logger := slog.New(slog.DiscardHandler)
	for _, tc := range []struct {
		name    string
		journal core.TaskNotificationJournal
		want    time.Time
	}{
		{name: "upcoming retry", journal: &retryClockJournal{next: now.Add(time.Second)}, want: now.Add(time.Second)},
		{name: "retry already due is the poll's", journal: &retryClockJournal{next: now.Add(-time.Second)}},
		{name: "unavailable", journal: &retryClockJournal{err: errors.New("db down")}},
		{name: "journal without a clock", journal: &schedulerNotificationJournal{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Scheduler{logger: logger, notifyJournal: tc.journal}
			if got := s.nextNotificationRetry(context.Background(), now); !got.Equal(tc.want) {
				t.Fatalf("next retry %s, want %s", got, tc.want)
			}
		})
	}
}
