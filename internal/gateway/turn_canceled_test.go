package gateway

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Two live turns were logged as "cortex: cannot resolve system prompt" on
// 2026-08-10, 400µs after the person pressed stop. The stop button had
// shipped that day. Nothing was wrong with the prompt: the turn's context
// was canceled, the persona lookup failed with it, and the abort was filed
// at Error — where the alerter picks it up, and where a genuine failure of
// the prompt stack becomes indistinguishable from someone changing their
// mind.
func TestTurnCanceledSeparatesAStoppedTurnFromABrokenOne(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	// How it actually arrives: wrapped twice on the way up from the driver.
	wrapped := fmt.Errorf("system prompt for soul X: persona lookup failed: %w", context.Canceled)

	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"stop pressed, error carries the cancellation", context.Background(), wrapped, true},
		{"stop pressed, driver reports its own error", canceled, errors.New("conn closed"), true},
		{"a real failure on a live context", context.Background(), errors.New("no persona row for soul"), false},
		// Timeouts stay Errors: a lookup that runs out of time is a fault,
		// and folding it in here would hide it behind an Info line.
		{"deadline exceeded is not a cancellation", context.Background(), context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := turnCanceled(tc.ctx, tc.err); got != tc.want {
				t.Errorf("turnCanceled = %v, want %v", got, tc.want)
			}
		})
	}
}
