package core

import (
	"testing"
	"time"
)

func TestTaskETAUsesDependenciesParallelismAndWaiting(t *testing.T) {
	now := time.Now()
	sample := func(id, kind string, deps ...string) stepTiming {
		return stepTiming{TaskStep: TaskStep{ID: id, Kind: kind, Status: "pending", Dependencies: deps}, Samples: 10, MedianSeconds: 60, HighSeconds: 120}
	}
	steps := []stepTiming{sample("a", "read"), sample("b", "read"), sample("c", "read"), sample("report", "finalize", "a", "b", "c")}
	low, high := estimateTaskETA("running", steps, now)
	if low == nil || *low != 120 || high == nil || *high != 480 {
		t.Fatal(low, high)
	}
	steps[1].NextAttemptAt = now.Add(90 * time.Second)
	low, high = estimateTaskETA("retry_wait", steps, now)
	if low == nil || *low != 210 || *high != 570 {
		t.Fatal(low, high)
	}
	steps[0].Status = "done"
	steps[1].NextAttemptAt = time.Time{}
	start := now.Add(-30 * time.Second)
	steps[1].Status = "running"
	steps[1].Attempts = 1
	steps[1].StartedAt = &start
	low, high = estimateTaskETA("running", steps, now)
	if low == nil || *low != 120 || *high != 330 {
		t.Fatal(low, high)
	}
}

func TestTaskETADoesNotInventUnknownDurations(t *testing.T) {
	now := time.Now()
	start := now.Add(-3 * time.Minute)
	base := stepTiming{TaskStep: TaskStep{ID: "a", Kind: "read", Status: "pending"}, Samples: 5, MedianSeconds: 60, HighSeconds: 120}
	for _, name := range []string{"queued", "samples", "blocked", "overdue", "missing dependency", "cycle", "done"} {
		t.Run(name, func(t *testing.T) {
			s := base
			stage := "running"
			switch name {
			case "queued":
				stage = "queued"
			case "samples":
				s.Samples = 4
			case "blocked":
				s.Status = "reconciliation"
			case "overdue":
				s.Status = "running"
				s.Attempts = 1
				s.StartedAt = &start
			case "missing dependency":
				s.Dependencies = []string{"missing"}
			case "cycle":
				s.Dependencies = []string{"a"}
			case "done":
				s.Status = "done"
			}
			if low, high := estimateTaskETA(stage, []stepTiming{s}, now); low != nil || high != nil {
				t.Fatal("invented ETA", low, high)
			}
		})
	}
}

func TestTaskETACountsRetriedStepInFull(t *testing.T) {
	now := time.Now()
	start := now.Add(-3 * time.Minute)
	s := stepTiming{TaskStep: TaskStep{ID: "a", Kind: "read", Status: "running", Attempts: 2, StartedAt: &start}, Samples: 5, MedianSeconds: 60, HighSeconds: 120}
	low, high := estimateTaskETA("running", []stepTiming{s}, now)
	if low == nil || *low != 60 || high == nil || *high != 120 {
		t.Fatal(low, high)
	}
}

func TestTaskETADeadlineBoundsEstimate(t *testing.T) {
	now := time.Now()
	deadline := now.Add(10 * time.Minute)
	ptr := func(n int) *int { return &n }
	for _, tc := range []struct {
		name, stage       string
		deadline          *time.Time
		low, high         *int
		wantLow, wantHigh *int
	}{
		{"deadline only", "planning", &deadline, nil, nil, nil, ptr(600)},
		{"history inside deadline", "running", &deadline, ptr(120), ptr(300), ptr(120), ptr(300)},
		{"history capped", "running", &deadline, ptr(900), ptr(1800), ptr(600), ptr(600)},
		{"no deadline", "running", nil, nil, nil, nil, nil},
		{"waiting for user", "waiting", &deadline, nil, nil, nil, nil},
		{"terminal", "terminal", &deadline, nil, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			low, high := boundTaskETA(tc.stage, tc.deadline, tc.low, tc.high, now)
			same := func(a, b *int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
			if !same(low, tc.wantLow) || !same(high, tc.wantHigh) {
				t.Fatalf("got %v %v", low, high)
			}
		})
	}
	past := now.Add(-time.Minute)
	if low, high := boundTaskETA("running", &past, nil, nil, now); low != nil || high != nil {
		t.Fatal("expired deadline invented a bound", low, high)
	}
}

func TestTaskETAUsesPooledHistoryForKindWithoutSamples(t *testing.T) {
	now := time.Now()
	read := stepTiming{TaskStep: TaskStep{ID: "read", Kind: "read", Status: "pending"}, Samples: 6, MedianSeconds: 240, HighSeconds: 360}
	report := stepTiming{TaskStep: TaskStep{ID: "report", Kind: "finalize", Status: "pending", Dependencies: []string{"read"}}, Samples: 1, MedianSeconds: 60, HighSeconds: 60, PoolSamples: 7, PoolMedianSeconds: 200, PoolHighSeconds: 350}
	low, high := estimateTaskETA("running", []stepTiming{read, report}, now)
	if low == nil || *low != 440 || high == nil || *high != 710 {
		t.Fatal(low, high)
	}
	report.PoolSamples = 4
	if low, high := estimateTaskETA("running", []stepTiming{read, report}, now); low != nil || high != nil {
		t.Fatal("estimate without enough pooled history", low, high)
	}
}

func TestTaskETAFinalCheckUsesMeasuredChecks(t *testing.T) {
	low, high := estimateFinalCheck("verifying", 60, 3, 203, 282)
	if low == nil || *low != 143 || high == nil || *high != 222 {
		t.Fatal(low, high)
	}
	if low, high := estimateFinalCheck("verifying", 600, 3, 203, 282); low == nil || *low != 60 || *high != 120 {
		t.Fatal("overdue check must still show a short remaining range", low, high)
	}
	if low, high := estimateFinalCheck("verifying", 0, 2, 203, 282); low != nil || high != nil {
		t.Fatal("estimate without enough checks", low, high)
	}
	if low, _ := estimateFinalCheck("terminal", 0, 5, 203, 282); low != nil {
		t.Fatal("finished task got an estimate")
	}
}
