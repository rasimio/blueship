// Package taskcontrol exposes owner-scoped task lifecycle controls to API hosts.
package taskcontrol

import "github.com/rasimio/blueship/internal/core"

type TaskController = core.TaskController

var NewTaskController = core.NewTaskController
var NewTaskControllerInSchema = core.NewTaskControllerInSchema

var ErrTaskNotTerminal = core.ErrTaskNotTerminal
var ErrTaskRestartUncertain = core.ErrTaskRestartUncertain
var ErrTaskNotRecurring = core.ErrTaskNotRecurring
