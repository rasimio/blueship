package core

import "context"

// TaskDeliveryValidator checks whether every referenced occurrence is still
// current immediately before its immutable notification is sent. It runs on
// initial sends and durable outbox retries, with the task's user/soul context.
// False permanently discards obsolete text; an error defers the send without
// calling the transport. Nil preserves the framework's usual delivery policy.
type TaskDeliveryValidator func(context.Context, AgentTask, []TaskDeliveryRef) (bool, error)
