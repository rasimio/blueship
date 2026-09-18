package agenttask

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

func TestTaskDeliveryValidationProtectsInitialAndRetrySend(t *testing.T) {
	for _, retry := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			current bool
			err     error
		}{
			{name: "current", current: true},
			{name: "stale"},
			{name: "lookup unavailable", err: errors.New("database unavailable")},
		} {
			path := "initial/"
			if retry {
				path = "retry/"
			}
			t.Run(path+tc.name, func(t *testing.T) {
				task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), SoulID: uuid.New()}
				refs := []core.TaskDeliveryRef{{InputID: "source", ItemKey: "occurrence:v1"}}
				checks, sends, confirms, rejects, defers := 0, 0, 0, 0, 0
				journal := &schedulerNotificationJournal{
					confirm:      func(context.Context, uuid.UUID, core.TaskNotificationReceipt) error { confirms++; return nil },
					reject:       func(context.Context, uuid.UUID, string) error { rejects++; return nil },
					deferAttempt: func(context.Context, uuid.UUID, string, time.Time) error { defers++; return nil },
				}
				s := &Scheduler{
					deps: &core.Deps{Config: &core.Config{TaskDeliveryValidator: func(ctx context.Context, got core.AgentTask, gotRefs []core.TaskDeliveryRef) (bool, error) {
						checks++
						if got.ID != task.ID || core.UserIDFromContext(ctx) != task.UserID || core.SoulIDFromContext(ctx) != task.SoulID || len(gotRefs) != 1 || gotRefs[0] != refs[0] {
							t.Fatalf("validator received wrong scope/task/refs: %+v, %+v", got, gotRefs)
						}
						return tc.current, tc.err
					}}},
					notify: func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
						sends++
						return core.TaskNotificationReceipt{}, nil
					},
					notifyJournal: journal,
					notifyTask:    func(context.Context, uuid.UUID) (core.AgentTask, error) { return task, nil },
				}
				var err error
				if retry {
					err = s.retryTaskNotification(context.Background(), core.TaskNotificationIntent{
						ID: uuid.New(), TaskID: task.ID, UserID: task.UserID, Text: "immutable", Refs: refs,
					})
				} else {
					_, err = deliverTaskNotification(context.Background(), s.validatedTaskNotifier(task, refs), journal,
						task.ID, task.UserID, "immutable", refs)
				}
				if checks != 1 {
					t.Fatalf("validator checks = %d", checks)
				}
				if tc.current {
					if err != nil || sends != 1 || confirms != 1 || rejects != 0 || defers != 0 {
						t.Fatalf("current delivery: err=%v sends=%d confirms=%d rejects=%d defers=%d", err, sends, confirms, rejects, defers)
					}
				} else if tc.err != nil {
					if !core.IsDefinitelyNotSent(err) || sends != 0 || confirms != 0 || defers != 1 || rejects != 0 {
						t.Fatalf("lookup outage: err=%v sends=%d confirms=%d rejects=%d defers=%d", err, sends, confirms, rejects, defers)
					}
				} else if !core.IsPermanentlyNotSent(err) || sends != 0 || confirms != 0 || rejects != 1 || defers != 0 {
					t.Fatalf("stale delivery: err=%v sends=%d confirms=%d rejects=%d defers=%d", err, sends, confirms, rejects, defers)
				}
			})
		}
	}
}

func TestTaskDeliveryRetryRechecksStateAfterTransportFailure(t *testing.T) {
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), SoulID: uuid.New()}
	refs := []core.TaskDeliveryRef{{InputID: "source", ItemKey: "old-occurrence"}}
	current := true
	sends, rejects, checks := 0, 0, 0
	journal := &schedulerNotificationJournal{reject: func(context.Context, uuid.UUID, string) error { rejects++; return nil }}
	s := &Scheduler{
		deps: &core.Deps{Config: &core.Config{TaskDeliveryValidator: func(context.Context, core.AgentTask, []core.TaskDeliveryRef) (bool, error) {
			checks++
			return current, nil
		}}},
		notify: func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
			sends++
			return core.TaskNotificationReceipt{}, core.DefinitelyNotSent(errors.New("transport unavailable"))
		},
		notifyJournal: journal,
		notifyTask:    func(context.Context, uuid.UUID) (core.AgentTask, error) { return task, nil },
	}
	_, err := deliverTaskNotification(context.Background(), s.validatedTaskNotifier(task, refs), journal, task.ID, task.UserID, "old text", refs)
	if !core.IsDefinitelyNotSent(err) || sends != 1 {
		t.Fatalf("initial send = %v, sends = %d", err, sends)
	}
	current = false // the source changed while its message waited in the outbox
	err = s.retryTaskNotification(context.Background(), core.TaskNotificationIntent{
		ID: uuid.New(), TaskID: task.ID, UserID: task.UserID, Text: "old text", Refs: refs,
	})
	if !core.IsPermanentlyNotSent(err) || checks != 2 || sends != 1 || rejects != 1 {
		t.Fatalf("stale retry: err=%v checks=%d sends=%d rejects=%d", err, checks, sends, rejects)
	}
}
