package handler

import (
	"github.com/rasimio/blueship/internal/core"
	"time"
)

// researchDeadline preserves a finalization window even when one tool-heavy
// iteration outlives its estimate. Explicit short deadlines keep at most one
// third for finalization; no deadline means no additional research timer.
func researchDeadline(task core.AgentTask, reserve time.Duration) *time.Time {
	if task.Deadline == nil {
		return nil
	}
	if reserve <= 0 {
		reserve = 3 * time.Minute
	}
	if !task.CreatedAt.IsZero() {
		total := task.Deadline.Sub(core.TaskWallStart(task, task.CreatedAt))
		if total > 0 && reserve > total/3 {
			reserve = total / 3
		}
	}
	deadline := task.Deadline.Add(-reserve)
	return &deadline
}
