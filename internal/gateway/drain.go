package gateway

// Drain-then-exit. A deploy restarts the daemon with kickstart -k, and
// until now every turn in flight died with the process: the LLM call
// was cancelled with the root context, the partial reply was recorded
// as interrupted, and the person was left mid-sentence. Make-before-
// break is not available — the daemon holds a single-instance lock so
// two of them cannot double-poll Telegram — so the achievable form is
// this: on shutdown the gateway stops taking new turns, lets the ones
// already running finish under their own context, and only then lets
// Run return. The host's stop timeout and launchd's exit timeout must
// both exceed the drain timeout, or the kill arrives first anyway.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// drainGuard counts turns in flight and waits for them at shutdown.
type drainGuard struct {
	wg    sync.WaitGroup
	count atomic.Int64
}

// begin registers one turn in flight; the returned func ends it.
func (d *drainGuard) begin() func() {
	d.wg.Add(1)
	d.count.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			d.count.Add(-1)
			d.wg.Done()
		})
	}
}

// inFlight is the current number of running turns.
func (d *drainGuard) inFlight() int64 { return d.count.Load() }

// wait blocks until every registered turn has ended or the timeout
// passes. It reports how many turns were still running when it gave
// up — zero means a clean drain.
func (d *drainGuard) wait(timeout time.Duration) int64 {
	if timeout <= 0 {
		return d.inFlight()
	}
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return 0
	case <-time.After(timeout):
		return d.inFlight()
	}
}

// turnContext is the context a turn runs under when draining is on: it
// does not die with the root context — the turn is owed its ending —
// but it still carries the root's values (tenant, tracing) and the
// gateway's own stop control still cancels it through beginTurn.
func turnContext(root context.Context, drain time.Duration) context.Context {
	if drain <= 0 {
		return root
	}
	return context.WithoutCancel(root)
}
