package core

import (
	"github.com/google/uuid"
	"testing"
)

func TestBackgroundExecutorRolloutOnlySelectsEligibleCanaryTasks(t *testing.T) {
	user := uuid.New()
	for _, tc := range []struct {
		name   string
		cfg    BackgroundTaskConfig
		change func(*AgentTask)
		want   int
	}{
		{name: "default off", want: 1},
		{name: "all enabled", cfg: BackgroundTaskConfig{ExecutorV2: true}, want: 2},
		{name: "included user", cfg: BackgroundTaskConfig{ExecutorV2: true, CanaryUserIDs: []string{user.String()}}, want: 2},
		{name: "excluded user", cfg: BackgroundTaskConfig{ExecutorV2: true, CanaryUserIDs: []string{uuid.NewString()}}, want: 1},
		{name: "invalid canary fails closed", cfg: BackgroundTaskConfig{ExecutorV2: true, CanaryUserIDs: []string{"*"}}, want: 1},
		{name: "scheduled", cfg: BackgroundTaskConfig{ExecutorV2: true}, change: func(task *AgentTask) { s := "@daily"; task.Schedule = &s }, want: 1},
		{name: "cadenced", cfg: BackgroundTaskConfig{ExecutorV2: true}, change: func(task *AgentTask) { s := "30m"; task.Cadence = &s }, want: 1},
		{name: "structured", cfg: BackgroundTaskConfig{ExecutorV2: true}, change: func(task *AgentTask) { task.Strategy = StrategyStructured }, want: 1},
		{name: "custom handler", cfg: BackgroundTaskConfig{ExecutorV2: true}, change: func(task *AgentTask) { task.Handler = "custom" }, want: 1},
		{name: "delegated", cfg: BackgroundTaskConfig{ExecutorV2: true}, change: func(task *AgentTask) { s := "peer"; task.DelegateTo = &s }, want: 1},
		{name: "assigned agents", cfg: BackgroundTaskConfig{ExecutorV2: true}, change: func(task *AgentTask) { task.UseAgents = []string{"peer"} }, want: 1},
		{name: "missing identity", cfg: BackgroundTaskConfig{ExecutorV2: true}, change: func(task *AgentTask) { task.UserID = uuid.Nil }, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := AgentTask{UserID: user, Strategy: StrategyDirect}
			if tc.change != nil {
				tc.change(&task)
			}
			if got := tc.cfg.ExecutorVersion(task); got != tc.want {
				t.Fatalf("version=%d want=%d", got, tc.want)
			}
		})
	}
}
