package core

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RecurringTaskWake lets a host run a recurring task before its schedule
// elapses, when work the task handles comes due between two scheduled runs:
// a reminder set for 19:42 under a 5-minute heartbeat that last ran at 19:39
// otherwise waits for the 19:44 run. Every other dispatch gate (quiet hours,
// start_at, daily_at, cadence, execution policy) still applies.
type RecurringTaskWake interface {
	// NextWake returns the earliest moment strictly after now at which some
	// recurring task will have such work, or the zero time when none is known.
	// The scheduler arms a timer for it, so a past moment is ignored.
	NextWake(ctx context.Context, now time.Time) (time.Time, error)
	// DueEarly returns the IDs, among tasks, whose work came due after the
	// task's last run and is due at now. Every task passed has a schedule
	// that has not elapsed yet and a non-nil LastRunAt.
	DueEarly(ctx context.Context, now time.Time, tasks []AgentTask) (map[uuid.UUID]bool, error)
}
