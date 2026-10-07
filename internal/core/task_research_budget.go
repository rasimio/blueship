package core

import (
	"encoding/json"
	"strings"
	"time"
)

// BackgroundParallelReads is the executor capacity exposed to planning.
const BackgroundParallelReads = 3

// TaskResearchDeadline reserves the same finalization window for planning and execution.
func TaskResearchDeadline(task AgentTask, reserve time.Duration) *time.Time {
	if TaskStopDeadline(task) == nil {
		return nil
	}
	if reserve <= 0 {
		reserve = 3 * time.Minute
	}
	if !task.CreatedAt.IsZero() {
		total := task.Deadline.Sub(task.CreatedAt)
		if total > 0 && reserve > total/3 {
			reserve = total / 3
		}
	}
	deadline := task.Deadline.Add(-reserve)
	return &deadline
}

// TaskStepHasCandidate identifies read work that can finish without new evidence.
// This permits synthesis/review only; the runtime must still deny late tools.
func TaskStepHasCandidate(step TaskStep) bool {
	if step.Kind != "read" {
		return false
	}
	var cp struct {
		CandidateReady bool   `json:"candidate_ready"`
		Output         string `json:"output"`
	}
	return json.Unmarshal(step.Checkpoint, &cp) == nil && cp.CandidateReady && strings.TrimSpace(cp.Output) != ""
}
