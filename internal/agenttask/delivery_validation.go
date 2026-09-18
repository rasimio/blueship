package agenttask

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

// A notification can wait in the outbox while the person changes its source.
// Validate at the send boundary, never only when the handler generated text.
func (s *Scheduler) validatedTaskNotifier(task core.AgentTask, refs []core.TaskDeliveryRef) func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
	if s.notify == nil || len(refs) == 0 || s.deps == nil || s.deps.Config == nil || s.deps.Config.TaskDeliveryValidator == nil {
		return s.notify
	}
	validate := s.deps.Config.TaskDeliveryValidator
	refs = append([]core.TaskDeliveryRef(nil), refs...)
	return func(ctx context.Context, userID uuid.UUID, text string) (core.TaskNotificationReceipt, error) {
		if userID != task.UserID {
			return core.TaskNotificationReceipt{}, core.PermanentlyNotSent(fmt.Errorf("delivery user does not match task user"))
		}
		ctx = core.WithUserID(core.WithSoulID(ctx, task.SoulID), task.UserID)
		current, err := validate(ctx, task, refs)
		if err != nil {
			return core.TaskNotificationReceipt{}, core.DefinitelyNotSent(fmt.Errorf("validate notification occurrences: %w", err))
		}
		if !current {
			return core.TaskNotificationReceipt{}, core.PermanentlyNotSent(fmt.Errorf("notification occurrences are no longer current"))
		}
		return s.notify(ctx, userID, text)
	}
}
