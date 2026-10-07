package core

import (
	"math"
	"time"
)

// StepTiming uses successful first attempts only: retry/backoff time is not a
// measurement of normal execution. These are planning bounds, not a confidence
// interval or a deadline guarantee.
type stepTiming struct {
	CheckpointPhase string `db:"checkpoint_phase"`
	TaskStep
	Samples       int     `db:"samples"`
	MedianSeconds float64 `db:"median_seconds"`
	HighSeconds   float64 `db:"high_seconds"`
	// All step kinds pooled: a rougher estimate while a kind (e.g. the
	// report) still lacks its own history.
	PoolSamples       int     `db:"pool_samples"`
	PoolMedianSeconds float64 `db:"pool_median_seconds"`
	PoolHighSeconds   float64 `db:"pool_high_seconds"`
}

// estimateTaskETA combines the critical path and available read parallelism for
// the lower bound; the upper bound allows serial execution at observed p90.
// Unknown admission waits, unresolved effects and anomalously long active steps
// deliberately suppress an estimate instead of promising an invented deadline.
func estimateTaskETA(stage string, steps []stepTiming, now time.Time) (*int, *int) {
	if stage != "running" && stage != "verifying" && stage != "finalizing" && stage != "retry_wait" && stage != "verification_wait" {
		return nil, nil
	}
	if len(steps) == 0 {
		return nil, nil
	}
	byID := map[string]stepTiming{}
	for _, s := range steps {
		byID[s.ID] = s
	}
	finished := map[string]float64{}
	visiting := map[string]bool{}
	var totalHigh, maxWait float64
	var reads, exclusive float64
	incomplete := 0
	for _, s := range steps {
		if s.Status == "done" {
			finished[s.ID] = 0
			continue
		}
		incomplete++
		if s.Samples < 5 && s.PoolSamples >= 5 {
			s.Samples, s.MedianSeconds, s.HighSeconds = s.PoolSamples, s.PoolMedianSeconds, s.PoolHighSeconds
		}
		if (s.Status != "pending" && s.Status != "running") || s.Samples < 5 || s.MedianSeconds <= 0 || s.HighSeconds < s.MedianSeconds {
			return nil, nil
		}
		// started_at covers the first attempt, so a retried step is counted as a
		// full step: its elapsed time in the current attempt is unknown.
		elapsed := 0.0
		if s.Status == "running" && s.Attempts == 1 && s.StartedAt != nil {
			elapsed = math.Max(0, now.Sub(*s.StartedAt).Seconds())
			if elapsed >= s.HighSeconds {
				return nil, nil
			}
		}
		s.MedianSeconds = math.Max(1, s.MedianSeconds-elapsed)
		s.HighSeconds = math.Max(s.MedianSeconds, s.HighSeconds-elapsed)
		byID[s.ID] = s
		totalHigh += s.HighSeconds
		maxWait = math.Max(maxWait, math.Max(0, s.NextAttemptAt.Sub(now).Seconds()))
		if s.Kind == "read" {
			reads += s.MedianSeconds
		} else {
			exclusive += s.MedianSeconds
		}
	}
	if incomplete == 0 {
		return nil, nil
	} // task-level acceptance may still be running
	var visit func(string) (float64, bool)
	visit = func(id string) (float64, bool) {
		if v, ok := finished[id]; ok {
			return v, true
		}
		s, ok := byID[id]
		if !ok || visiting[id] {
			return 0, false
		}
		visiting[id] = true
		ready := math.Max(0, s.NextAttemptAt.Sub(now).Seconds())
		for _, dep := range s.Dependencies {
			v, ok := visit(dep)
			if !ok {
				return 0, false
			}
			ready = math.Max(ready, v)
		}
		delete(visiting, id)
		finished[id] = ready + s.MedianSeconds
		return finished[id], true
	}
	critical := 0.0
	for _, s := range steps {
		v, ok := visit(s.ID)
		if !ok {
			return nil, nil
		}
		critical = math.Max(critical, v)
	}
	// GraphExecutor currently admits three read workers and exclusive actions.
	lower := int(math.Ceil(math.Max(critical, exclusive+reads/3)))
	upper := int(math.Ceil(math.Max(float64(lower), totalHigh+maxWait)))
	return &lower, &upper
}

// boundTaskETA caps an estimate at the task deadline, by which the executor
// finalizes whatever it has. Without enough measured history the deadline
// alone is still an honest "no later than" bound (nil lower bound).
func boundTaskETA(stage string, deadline *time.Time, low, high *int, now time.Time) (*int, *int) {
	if stage == "terminal" || stage == "waiting" || deadline == nil {
		return low, high
	}
	left := int(math.Ceil(deadline.Sub(now).Seconds()))
	if left <= 0 {
		return low, high
	}
	if high == nil || *high > left {
		high = &left
	}
	if low != nil && *low > *high {
		low = high
	}
	return low, high
}

// estimateFinalCheck covers the report's final verification, after every
// step is done: measured check durations minus the time already spent there.
func estimateFinalCheck(stage string, elapsed float64, samples int, median, high float64) (*int, *int) {
	if stage != "running" && stage != "verifying" && stage != "finalizing" && stage != "retry_wait" && stage != "verification_wait" {
		return nil, nil
	}
	if samples < 3 || median <= 0 {
		return nil, nil
	}
	low := int(math.Ceil(math.Max(60, median-elapsed)))
	up := int(math.Ceil(math.Max(float64(low+60), high-elapsed)))
	return &low, &up
}
