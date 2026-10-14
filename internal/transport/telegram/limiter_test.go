package telegram

import (
	"context"
	"testing"
	"time"
)

func fakeLimiter(start time.Time) (*sendLimiter, *time.Time) {
	now := start
	l := newSendLimiter()
	l.now = func() time.Time { return now }
	return l, &now
}

func TestLimiterConversationTakesTheWholeBurst(t *testing.T) {
	l, _ := fakeLimiter(time.Unix(1000, 0))
	for i := 0; i < sendBurst; i++ {
		if d := l.take(false); d != 0 {
			t.Fatalf("send %d waited %v inside the burst", i, d)
		}
	}
	if d := l.take(false); d <= 0 {
		t.Fatal("a send past the burst went without waiting")
	}
}

func TestLimiterBulkLeavesTheReserve(t *testing.T) {
	l, _ := fakeLimiter(time.Unix(1000, 0))
	taken := 0
	for l.take(true) == 0 {
		taken++
		if taken > sendBurst {
			t.Fatal("bulk never stopped")
		}
	}
	if taken != sendBurst-bulkReserve {
		t.Fatalf("bulk took %d tokens, want %d", taken, sendBurst-bulkReserve)
	}
	// The reserve is still there for a reply.
	for i := 0; i < bulkReserve; i++ {
		if d := l.take(false); d != 0 {
			t.Fatalf("reply %d waited %v with the reserve intact", i, d)
		}
	}
}

func TestLimiterRefillsAtTheRate(t *testing.T) {
	l, now := fakeLimiter(time.Unix(1000, 0))
	for l.take(false) == 0 {
	}
	*now = now.Add(time.Second)
	got := 0
	for l.take(false) == 0 {
		got++
	}
	if got != sendRatePerSecond {
		t.Fatalf("a second refilled %d tokens, want %d", got, sendRatePerSecond)
	}
}

func TestLimiterPauseHoldsBulkOnly(t *testing.T) {
	l, now := fakeLimiter(time.Unix(1000, 0))
	l.pause(3 * time.Second)
	if d := l.take(true); d != 3*time.Second {
		t.Fatalf("bulk wait = %v, want the full retry_after", d)
	}
	if d := l.take(false); d != 0 {
		t.Fatalf("a reply waited %v under a bulk pause", d)
	}
	*now = now.Add(3 * time.Second)
	if d := l.take(true); d != 0 {
		t.Fatalf("bulk still held %v after retry_after elapsed", d)
	}
}

func TestLimiterWaitStopsWithContext(t *testing.T) {
	l, _ := fakeLimiter(time.Unix(1000, 0))
	l.pause(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.wait(ctx, true); err == nil {
		t.Fatal("wait outlived its context")
	}
}

func TestNilLimiterPacesNothing(t *testing.T) {
	var l *sendLimiter
	if err := l.wait(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	l.pause(time.Second)
}

func TestBulkContextMarksOnlyItsSends(t *testing.T) {
	ctx := context.Background()
	if isBulk(ctx) {
		t.Fatal("plain context reads as bulk")
	}
	if !isBulk(WithBulk(ctx)) {
		t.Fatal("WithBulk not read back")
	}
}
