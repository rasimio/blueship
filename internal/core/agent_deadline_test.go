package core

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTaskWallDeadlineIncludesQueueAndRestarts(t *testing.T) {
	created := time.Date(2026, 8, 27, 19, 40, 0, 0, time.UTC)
	task := AgentTask{Strategy: StrategyDirect, CreatedAt: created}
	first := TaskWallDeadline(task, created, 0)
	if !first.Equal(created.Add(30 * time.Minute)) {
		t.Fatal(first)
	}
	// A restart twenty-one hours later must not grant another thirty minutes.
	if got := TaskWallDeadline(task, created.Add(21*time.Hour), 0); !got.Equal(*first) {
		t.Fatal(got)
	}
	task.Deadline = first
	if got := TaskWallDeadline(task, created.Add(21*time.Hour), time.Hour); !got.Equal(*first) {
		t.Fatal("configuration/restart extended an already persisted deadline")
	}
}

func TestTaskWallDeadlineDelayedExplicitAndRecurring(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	task := AgentTask{Strategy: StrategyDirect, CreatedAt: now,
		Config: json.RawMessage(`{"start_at":"2026-09-18T12:00:00Z"}`)}
	if got := TaskWallDeadline(task, now, 0); !got.Equal(now.Add(24*time.Hour + 30*time.Minute)) {
		t.Fatal(got)
	}
	explicit := now.Add(24*time.Hour + 5*time.Minute)
	task.Deadline = &explicit
	if got := TaskWallDeadline(task, now, 0); !got.Equal(explicit) {
		t.Fatal(got)
	}
	// Date precision matters: do not consume the runtime allowance before start_at.
	if got := TaskWallStart(task, now); !got.Equal(now.Add(24 * time.Hour)) {
		t.Fatal(got)
	}
	task.Deadline = nil
	cadence := "1h"
	task.Cadence = &cadence
	if got := TaskWallDeadline(task, now, 0); got != nil {
		t.Fatal("periodic monitor was capped")
	}
	task.Cadence = nil
	schedule := "5m"
	task.Schedule = &schedule
	if got := TaskWallDeadline(task, now, 0); got != nil {
		t.Fatal("heartbeat was capped")
	}
}

func TestGraphOneShotTasksRunToCompletion(t *testing.T) {
	now := time.Now()
	stored := now.Add(-time.Minute)
	oneShot := AgentTask{ExecutorVersion: 2, Strategy: StrategyDirect, CreatedAt: now.Add(-time.Hour), Deadline: &stored}
	if !TaskRunsToCompletion(oneShot) || TaskStopDeadline(oneShot) != nil || TaskWallDeadline(oneShot, now, time.Minute) != nil || TaskResearchDeadline(oneShot, time.Minute) != nil {
		t.Fatal("one-shot graph task still has a wall-clock stop")
	}
	legacy := oneShot
	legacy.ExecutorVersion = 1
	if TaskRunsToCompletion(legacy) || TaskStopDeadline(legacy) == nil || TaskWallDeadline(legacy, now, time.Minute) == nil {
		t.Fatal("legacy task lost its limit")
	}
	recurring := oneShot
	recurring.Strategy = StrategyRecurring
	if TaskRunsToCompletion(recurring) || TaskStopDeadline(recurring) == nil {
		t.Fatal("recurring task lost its limit")
	}
}

func TestGraphReportIsDoneEvenWithLimitations(t *testing.T) {
	oneShot := AgentTask{ExecutorVersion: 2, Strategy: StrategyDirect}
	legacy := AgentTask{ExecutorVersion: 1, Strategy: StrategyDirect}
	for _, tc := range []struct {
		task    AgentTask
		outcome string
		want    string
	}{
		{oneShot, "completed", "done"}, {oneShot, "partial", "done"}, {oneShot, "blocked", "failed"}, {oneShot, "cancelled", "canceled"},
		{legacy, "partial", "failed"}, {legacy, "completed", "done"},
	} {
		if got := TaskTerminalStatus(tc.task, tc.outcome); got != tc.want {
			t.Fatalf("v%d %s: %s", tc.task.ExecutorVersion, tc.outcome, got)
		}
	}
}
