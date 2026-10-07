package looprunner

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestQueueWakeupRunsBeforePollAndClosedChannelsStayIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake := make(chan struct{}, 1)
	peer := make(chan string, 1)
	calls := make(chan string, 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunLoopWithWakeup(ctx, slog.New(slog.DiscardHandler), "test", time.Hour, func(context.Context) error { calls <- "scan"; return nil }, peer, func(_ context.Context, value string) { calls <- value }, wake)
	}()
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-calls:
			if got != want {
				t.Fatalf("got %s want %s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("scheduler did not wake")
		}
	}
	expect("scan")
	wake <- struct{}{}
	expect("scan")
	peer <- "peer-id"
	expect("peer-id")
	expect("scan")
	close(wake)
	close(peer)
	select {
	case got := <-calls:
		t.Fatal("closed channel caused busy loop", got)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop")
	}
}
