// Package taskcreation shares background execution policy with API hosts.
package taskcreation

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

type Config = core.BackgroundTaskConfig

// Execution fixes the executor and absolute deadline at creation. A disabled
// creation flag never changes the version of an existing task.
func Execution(cfg Config, user uuid.UUID, strategy, schedule, startAt string, now time.Time) (int, *time.Time) {
	raw, _ := json.Marshal(map[string]string{"start_at": startAt})
	task := core.AgentTask{UserID: user, Handler: "background", Strategy: strategy, Config: raw, CreatedAt: now}
	if schedule != "" {
		task.Schedule = &schedule
	}
	task.ExecutorVersion = cfg.ExecutorVersion(task)
	return task.ExecutorVersion, core.TaskWallDeadline(task, now, core.DefaultTaskWallTimeout)
}
