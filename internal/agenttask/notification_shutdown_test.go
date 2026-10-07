package agenttask

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

func TestNotificationDrainsCoalesceWithoutBlockingTaskDispatch(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var claims atomic.Int32
	s := &Scheduler{notifyJournal: &schedulerNotificationJournal{claim: func(context.Context, time.Time) (*core.TaskNotificationIntent, error) {
		if claims.Add(1) == 1 {
			close(started)
			<-release
		}
		return nil, nil
	}}}
	s.drainNotificationsAsync(context.Background())
	<-started
	returned := make(chan struct{})
	go func() {
		for range 32 {
			s.drainNotificationsAsync(context.Background())
		}
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("delivery kicks blocked the caller")
	}
	if claims.Load() != 1 {
		close(release)
		t.Fatal("concurrent outbox workers", claims.Load())
	}
	close(release)
	s.Wait()
	if claims.Load() != 2 {
		t.Fatal("kicks during delivery must coalesce into one fresh pass", claims.Load())
	}
	s.drainNotificationsAsync(context.Background())
	s.Wait()
	if claims.Load() != 3 {
		t.Fatal("worker did not restart after becoming idle", claims.Load())
	}
}

func TestSchedulerWaitDrainsPostFinalizationNotifications(t *testing.T) {
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s := &Scheduler{notifyJournal: &schedulerNotificationJournal{claim: func(context.Context, time.Time) (*core.TaskNotificationIntent, error) {
		close(started)
		<-release
		return nil, nil
	}}}
	s.drainNotificationsAsync(context.Background())
	<-started
	go func() { s.Wait(); close(finished) }()
	select {
	case <-finished:
		t.Fatal("shutdown returned while outbox worker still used database")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after outbox drain")
	}
}
