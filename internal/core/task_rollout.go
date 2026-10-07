package core

import "github.com/google/uuid"

// BackgroundTaskConfig gates creation only. Turning it off must not prevent
// dispatch/recovery of tasks whose executor version is already persisted.
type BackgroundTaskConfig struct {
	ExecutorV2 bool `yaml:"executor_v2" json:"executor_v2"`
	// An empty allowlist enables all eligible users when ExecutorV2 is true.
	CanaryUserIDs []string `yaml:"canary_user_ids" json:"canary_user_ids"`
}

func (c BackgroundTaskConfig) ExecutorVersion(task AgentTask) int {
	if !c.ExecutorV2 || !graphTaskEligible(task) || task.UserID == uuid.Nil {
		return 1
	}
	if len(c.CanaryUserIDs) == 0 {
		return 2
	}
	for _, id := range c.CanaryUserIDs {
		if id == task.UserID.String() {
			return 2
		}
	}
	return 1
}

func graphTaskEligible(task AgentTask) bool {
	return task.Strategy == StrategyDirect && (task.Handler == "" || task.Handler == "background") && task.Schedule == nil && task.Cadence == nil && task.DelegateTo == nil && len(task.UseAgents) == 0
}
