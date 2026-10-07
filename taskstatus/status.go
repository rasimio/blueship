// Package taskstatus exposes the canonical read-only task projection to API
// hosts without importing the ship lifecycle, transports and tool integrations.
package taskstatus

import "github.com/rasimio/blueship/internal/core"

type TaskStatus = core.TaskStatus
type TaskStatusReader = core.TaskStatusReader

var NewTaskStatusReader = core.NewTaskStatusReader
var NewTaskStatusReaderInSchema = core.NewTaskStatusReaderInSchema
