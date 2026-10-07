package agenttask

import (
	"context"
	"testing"
	"time"
)

func TestWakeTimerKeepsEarliestBoundaryAndStops(t *testing.T) {
	events := make(chan struct{}, 10)
	s := &Scheduler{wakeup: func() { events <- struct{}{} }}
	defer s.StopWakeups()
	now := time.Now()
	s.scheduleWakeupAt(context.Background(), now.Add(500*time.Millisecond))
	s.scheduleWakeupAt(context.Background(), now.Add(40*time.Millisecond))
	s.scheduleWakeupAt(context.Background(), now.Add(700*time.Millisecond))
	select {
	case <-events:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("later hint postponed earliest wake")
	}
	s.scheduleWakeupAt(context.Background(), time.Now().Add(40*time.Millisecond))
	s.StopWakeups()
	select {
	case <-events:
		t.Fatal("stopped timer woke scheduler")
	case <-time.After(80 * time.Millisecond):
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.scheduleWakeupAt(ctx, time.Now().Add(40*time.Millisecond))
	cancel()
	select {
	case <-events:
		t.Fatal("cancelled scheduler woke")
	case <-time.After(80 * time.Millisecond):
	}
}
