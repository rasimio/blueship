package agenttask

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"
)

// Run's first act is requeueing whatever the previous process abandoned in
// 'running'. Production earned this twice in two days: a deploy killed the
// daemon seconds after a claim, the task sat invisible to PendingTasks, and
// the periodic detector's 10-minute hang threshold read as silent limbo —
// an operator requeued by hand and recorded "there is no reaper" as fact.
//
// The scheduler under test has no store, so Run panics somewhere after the
// reap; the recover is deliberate — the claim is only about what happens
// FIRST, and tolerating the later panic keeps the test free of a database.
func TestRunReapsOrphansFirstAndOnlyOnce(t *testing.T) {
	calls := 0
	var gotThreshold time.Duration
	s := &Scheduler{
		logger: slog.Default(),
		resetStale: func(_ context.Context, staleAfter time.Duration) (int64, error) {
			calls++
			gotThreshold = staleAfter
			return 2, nil
		},
	}

	runSwallowingPanic := func() {
		defer func() { _ = recover() }()
		_ = s.Run(context.Background())
	}

	runSwallowingPanic()
	if calls != 1 {
		t.Fatalf("boot reap ran %d times on the first tick, want 1", calls)
	}
	// Zero threshold is the point: at boot, 'running' can only mean
	// orphaned — this process has claimed nothing and no other exists.
	// A hang threshold here recreates the 10-minute limbo.
	if gotThreshold != 0 {
		t.Fatalf("boot reap used a hang threshold of %v; at boot every running task is already an orphan", gotThreshold)
	}

	runSwallowingPanic()
	if calls != 1 {
		t.Fatalf("boot reap ran again on a later tick (%d calls); it must be once per process", calls)
	}
}

// A failed lifecycle write may not be quiet: every orphan it missed stays
// invisible until the 10-minute detector, which is the exact limbo the boot
// reap exists to remove.
func TestBootReapFailureIsLoud(t *testing.T) {
	var buf bytes.Buffer
	s := &Scheduler{
		logger: slog.New(slog.NewTextHandler(&buf, nil)),
		resetStale: func(context.Context, time.Duration) (int64, error) {
			return 0, context.DeadlineExceeded
		},
	}
	s.reapOrphansAtBoot(context.Background())
	if !bytes.Contains(buf.Bytes(), []byte("boot reap failed")) {
		t.Fatalf("reap failure left no ERROR in the log:\n%s", buf.String())
	}
}

// No store and no seam must not crash the scheduler at boot — hosts that
// construct a bare Scheduler in tests rely on Run being callable.
func TestBootReapToleratesAMissingStore(t *testing.T) {
	s := &Scheduler{logger: slog.Default()}
	s.reapOrphansAtBoot(context.Background())
}
