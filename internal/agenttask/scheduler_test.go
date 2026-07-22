package agenttask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/internal/core"
)

type recordingMCPSource struct {
	seen  []uuid.UUID
	tools []core.MCPTool
}

func (s *recordingMCPSource) ToolsForSoul(_ context.Context, soulID uuid.UUID) []core.MCPTool {
	s.seen = append(s.seen, soulID)
	return s.tools
}

func (*recordingMCPSource) Invalidate(uuid.UUID) {}

func noopTool(context.Context, json.RawMessage) (any, error) { return map[string]any{"ok": true}, nil }

type schedulerHandlerFunc func(context.Context, core.AgentTask, core.AgentDeps) (core.IterationResult, error)

func (f schedulerHandlerFunc) Run(ctx context.Context, task core.AgentTask, deps core.AgentDeps) (core.IterationResult, error) {
	return f(ctx, task, deps)
}

func (schedulerHandlerFunc) DefaultTools() []string { return nil }

type schedulerNotificationJournal struct {
	begin        func(context.Context, uuid.UUID, uuid.UUID, string, []core.TaskDeliveryRef) (uuid.UUID, bool, error)
	confirm      func(context.Context, uuid.UUID, core.TaskNotificationReceipt) error
	uncertain    func(context.Context, uuid.UUID, string) error
	deferAttempt func(context.Context, uuid.UUID, string, time.Time) error
	reject       func(context.Context, uuid.UUID, string) error
	claim        func(context.Context, time.Time) (*core.TaskNotificationIntent, error)
}

func (j *schedulerNotificationJournal) BeginNotificationAttempt(
	ctx context.Context,
	taskID, userID uuid.UUID,
	text string,
	refs []core.TaskDeliveryRef,
) (uuid.UUID, bool, error) {
	if j.begin == nil {
		return uuid.New(), true, nil
	}
	return j.begin(ctx, taskID, userID, text, refs)
}

func (j *schedulerNotificationJournal) ConfirmNotificationAttempt(
	ctx context.Context,
	id uuid.UUID,
	receipt core.TaskNotificationReceipt,
) error {
	if j.confirm == nil {
		return nil
	}
	return j.confirm(ctx, id, receipt)
}

func (j *schedulerNotificationJournal) MarkNotificationUncertain(ctx context.Context, id uuid.UUID, reason string) error {
	if j.uncertain == nil {
		return nil
	}
	return j.uncertain(ctx, id, reason)
}

func (j *schedulerNotificationJournal) DeferNotificationAttempt(ctx context.Context, id uuid.UUID, reason string, retryAt time.Time) error {
	if j.deferAttempt == nil {
		return nil
	}
	return j.deferAttempt(ctx, id, reason, retryAt)
}

func (j *schedulerNotificationJournal) RejectNotificationAttempt(ctx context.Context, id uuid.UUID, reason string) error {
	if j.reject == nil {
		return nil
	}
	return j.reject(ctx, id, reason)
}

func (j *schedulerNotificationJournal) ClaimRetryableNotification(ctx context.Context, now time.Time) (*core.TaskNotificationIntent, error) {
	if j.claim == nil {
		return nil, nil
	}
	return j.claim(ctx, now)
}

func TestDeliverTaskNotificationJournal(t *testing.T) {
	taskID, userID := uuid.New(), uuid.New()
	refs := []core.TaskDeliveryRef{{InputID: "calendar", ItemKey: "event:1"}}
	receipt := core.TaskNotificationReceipt{
		Transport: "telegram",
		BotID:     "bot-1",
		ChatID:    "42",
		MessageID: "99",
	}

	t.Run("success orders begin notify confirm", func(t *testing.T) {
		var order []string
		attemptID := uuid.New()
		journal := &schedulerNotificationJournal{
			begin: func(_ context.Context, gotTaskID, gotUserID uuid.UUID, gotText string, got []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
				order = append(order, "begin")
				if gotTaskID != taskID || gotUserID != userID || gotText != "soon" {
					t.Fatalf("begin args task=%s user=%s text=%q", gotTaskID, gotUserID, gotText)
				}
				if len(got) != 1 || got[0] != refs[0] {
					t.Fatalf("reserved refs = %#v, want %#v", got, refs)
				}
				return attemptID, true, nil
			},
			confirm: func(_ context.Context, gotID uuid.UUID, gotReceipt core.TaskNotificationReceipt) error {
				order = append(order, "confirm")
				if gotID != attemptID || gotReceipt != receipt {
					t.Fatalf("confirm id=%s receipt=%#v", gotID, gotReceipt)
				}
				return nil
			},
		}
		outcome, err := deliverTaskNotification(
			context.Background(),
			func(ctx context.Context, _ uuid.UUID, _ string) (core.TaskNotificationReceipt, error) {
				order = append(order, "notify")
				if !core.SingleAttemptNotificationFromContext(ctx) {
					t.Fatal("keyed transport was not marked single-attempt")
				}
				return receipt, nil
			},
			journal, taskID, userID, "soon", refs,
		)
		if err != nil || !outcome.Handled || !outcome.Delivered || strings.Join(order, ",") != "begin,notify,confirm" {
			t.Fatalf("outcome=%+v err=%v order=%v", outcome, err, order)
		}
	})

	t.Run("existing reservation forbids notify", func(t *testing.T) {
		notifyCalls := 0
		journal := &schedulerNotificationJournal{begin: func(context.Context, uuid.UUID, uuid.UUID, string, []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
			return uuid.New(), false, nil
		}}
		outcome, err := deliverTaskNotification(
			context.Background(),
			func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
				notifyCalls++
				return receipt, nil
			},
			journal, taskID, userID, "soon", refs,
		)
		if err != nil || !outcome.Handled || outcome.Delivered || notifyCalls != 0 {
			t.Fatalf("outcome=%+v err=%v notify_calls=%d", outcome, err, notifyCalls)
		}
	})

	t.Run("begin failure forbids notify and never reruns task", func(t *testing.T) {
		notifyCalls := 0
		journal := &schedulerNotificationJournal{begin: func(context.Context, uuid.UUID, uuid.UUID, string, []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
			return uuid.Nil, false, errors.New("database down")
		}}
		outcome, err := deliverTaskNotification(
			context.Background(),
			func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
				notifyCalls++
				return receipt, nil
			},
			journal, taskID, userID, "soon", refs,
		)
		if err == nil || !strings.Contains(err.Error(), "begin notification attempt") || !outcome.Handled || outcome.Delivered || notifyCalls != 0 {
			t.Fatalf("outcome=%+v err=%v notify_calls=%d", outcome, err, notifyCalls)
		}
	})

	t.Run("missing journal never sends or reruns keyed task", func(t *testing.T) {
		notifyCalls := 0
		outcome, err := deliverTaskNotification(
			context.Background(),
			func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
				notifyCalls++
				return receipt, nil
			},
			nil, taskID, userID, "soon", refs,
		)
		if err == nil || !strings.Contains(err.Error(), "journal unavailable") || !outcome.Handled || outcome.Delivered || notifyCalls != 0 {
			t.Fatalf("outcome=%+v err=%v notify_calls=%d", outcome, err, notifyCalls)
		}
	})

	t.Run("notify error is tombstoned uncertain", func(t *testing.T) {
		var order []string
		attemptID := uuid.New()
		journal := &schedulerNotificationJournal{
			begin: func(context.Context, uuid.UUID, uuid.UUID, string, []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
				order = append(order, "begin")
				return attemptID, true, nil
			},
			uncertain: func(_ context.Context, gotID uuid.UUID, reason string) error {
				order = append(order, "uncertain")
				if gotID != attemptID || !strings.Contains(reason, "connection reset") {
					t.Fatalf("uncertain id=%s reason=%q", gotID, reason)
				}
				return nil
			},
			confirm: func(context.Context, uuid.UUID, core.TaskNotificationReceipt) error {
				t.Fatal("confirm called after transport error")
				return nil
			},
		}
		outcome, err := deliverTaskNotification(
			context.Background(),
			func(ctx context.Context, _ uuid.UUID, _ string) (core.TaskNotificationReceipt, error) {
				order = append(order, "notify")
				if !core.SingleAttemptNotificationFromContext(ctx) {
					t.Fatal("keyed transport was not marked single-attempt")
				}
				return core.TaskNotificationReceipt{}, errors.New("connection reset")
			},
			journal, taskID, userID, "soon", refs,
		)
		if err == nil || !outcome.Handled || outcome.Delivered || strings.Join(order, ",") != "begin,notify,uncertain" {
			t.Fatalf("outcome=%+v err=%v order=%v", outcome, err, order)
		}
	})

	t.Run("definitely not sent defers immutable intent", func(t *testing.T) {
		var order []string
		attemptID := uuid.New()
		before := time.Now()
		journal := &schedulerNotificationJournal{
			begin: func(context.Context, uuid.UUID, uuid.UUID, string, []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
				order = append(order, "begin")
				return attemptID, true, nil
			},
			deferAttempt: func(_ context.Context, gotID uuid.UUID, reason string, retryAt time.Time) error {
				order = append(order, "defer")
				if gotID != attemptID || !strings.Contains(reason, "rate limited") {
					t.Fatalf("defer id=%s reason=%q", gotID, reason)
				}
				if retryAt.Before(before.Add(89*time.Second)) || retryAt.After(time.Now().Add(91*time.Second)) {
					t.Fatalf("retry_at=%s, want provider delay near 90s", retryAt)
				}
				return nil
			},
			uncertain: func(context.Context, uuid.UUID, string) error {
				t.Fatal("definitely-not-sent error was marked uncertain")
				return nil
			},
			confirm: func(context.Context, uuid.UUID, core.TaskNotificationReceipt) error {
				t.Fatal("definitely-not-sent error was confirmed")
				return nil
			},
		}
		outcome, err := deliverTaskNotification(
			context.Background(),
			func(ctx context.Context, _ uuid.UUID, _ string) (core.TaskNotificationReceipt, error) {
				order = append(order, "notify")
				if !core.SingleAttemptNotificationFromContext(ctx) {
					t.Fatal("keyed transport was not marked single-attempt")
				}
				return core.TaskNotificationReceipt{}, core.DefinitelyNotSentAfter(errors.New("rate limited"), 90*time.Second)
			},
			journal, taskID, userID, "soon", refs,
		)
		if err == nil || !outcome.Handled || outcome.Delivered || !core.IsDefinitelyNotSent(err) || strings.Join(order, ",") != "begin,notify,defer" {
			t.Fatalf("outcome=%+v err=%v order=%v", outcome, err, order)
		}
	})

	t.Run("permanently not sent rejects immutable intent", func(t *testing.T) {
		attemptID := uuid.New()
		rejected := false
		journal := &schedulerNotificationJournal{
			begin: func(context.Context, uuid.UUID, uuid.UUID, string, []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
				return attemptID, true, nil
			},
			reject: func(_ context.Context, gotID uuid.UUID, _ string) error {
				rejected = true
				if gotID != attemptID {
					t.Fatalf("reject id=%s, want %s", gotID, attemptID)
				}
				return nil
			},
		}
		outcome, err := deliverTaskNotification(
			context.Background(),
			func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
				return core.TaskNotificationReceipt{}, core.PermanentlyNotSent(errors.New("bot blocked"))
			},
			journal, taskID, userID, "soon", refs,
		)
		if err == nil || !core.IsPermanentlyNotSent(err) || !outcome.Handled || outcome.Delivered || !rejected {
			t.Fatalf("outcome=%+v err=%v rejected=%v", outcome, err, rejected)
		}
	})

	t.Run("missing sender after begin defers without rerunning task", func(t *testing.T) {
		attemptID := uuid.New()
		deferred := false
		journal := &schedulerNotificationJournal{
			begin: func(context.Context, uuid.UUID, uuid.UUID, string, []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
				return attemptID, true, nil
			},
			deferAttempt: func(_ context.Context, gotID uuid.UUID, reason string, retryAt time.Time) error {
				deferred = true
				if gotID != attemptID || !strings.Contains(reason, "sender unavailable") || time.Until(retryAt) < 59*time.Second {
					t.Fatalf("defer id=%s reason=%q retry_at=%s", gotID, reason, retryAt)
				}
				return nil
			},
		}
		outcome, err := deliverTaskNotification(context.Background(), nil, journal, taskID, userID, "soon", refs)
		if err == nil || !outcome.Handled || outcome.Delivered || !deferred {
			t.Fatalf("outcome=%+v err=%v deferred=%v", outcome, err, deferred)
		}
	})

	t.Run("transport gets fresh deadline and preserves values", func(t *testing.T) {
		soulID := uuid.New()
		baseCtx, cancelBase := context.WithCancel(core.WithSoulID(context.Background(), soulID))
		journal := &schedulerNotificationJournal{begin: func(context.Context, uuid.UUID, uuid.UUID, string, []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
			cancelBase() // Simulate the reservation consuming/cancelling its DB budget.
			return uuid.New(), true, nil
		}}
		outcome, err := deliverTaskNotification(
			baseCtx,
			func(ctx context.Context, _ uuid.UUID, _ string) (core.TaskNotificationReceipt, error) {
				if ctx.Err() != nil {
					t.Fatalf("transport inherited spent DB context: %v", ctx.Err())
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) < 9*time.Second {
					t.Fatalf("transport deadline = %v, want fresh ~10s", deadline)
				}
				if got := core.SoulIDFromContext(ctx); got != soulID {
					t.Fatalf("transport soul = %s, want %s", got, soulID)
				}
				return receipt, nil
			},
			journal, taskID, userID, "soon", refs,
		)
		if err != nil || !outcome.Handled || !outcome.Delivered {
			t.Fatalf("outcome=%+v err=%v", outcome, err)
		}
	})

	t.Run("confirm failure reservation suppresses retry", func(t *testing.T) {
		attemptID := uuid.New()
		begun := false
		notifyCalls := 0
		confirmCalls := 0
		journal := &schedulerNotificationJournal{
			begin: func(context.Context, uuid.UUID, uuid.UUID, string, []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
				if begun {
					return attemptID, false, nil
				}
				begun = true
				return attemptID, true, nil
			},
			confirm: func(context.Context, uuid.UUID, core.TaskNotificationReceipt) error {
				confirmCalls++
				return errors.New("commit failed")
			},
		}
		notify := func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
			notifyCalls++
			return receipt, nil
		}
		outcome, err := deliverTaskNotification(context.Background(), notify, journal, taskID, userID, "soon", refs)
		if err == nil || !strings.Contains(err.Error(), "confirm notification attempt") || !outcome.Handled || !outcome.Delivered {
			t.Fatalf("first attempt outcome=%+v err=%v", outcome, err)
		}
		outcome, err = deliverTaskNotification(context.Background(), notify, journal, taskID, userID, "soon", refs)
		if err != nil || !outcome.Handled || outcome.Delivered || notifyCalls != 1 || confirmCalls != 1 {
			t.Fatalf("retry outcome=%+v err=%v notify_calls=%d confirm_calls=%d", outcome, err, notifyCalls, confirmCalls)
		}
	})

	t.Run("ordinary notification path is unchanged", func(t *testing.T) {
		notifyCalls := 0
		outcome, err := deliverTaskNotification(
			context.Background(),
			func(ctx context.Context, _ uuid.UUID, _ string) (core.TaskNotificationReceipt, error) {
				notifyCalls++
				if core.SingleAttemptNotificationFromContext(ctx) {
					t.Fatal("unkeyed transport unexpectedly marked single-attempt")
				}
				return receipt, nil
			},
			nil, taskID, userID, "ordinary", nil,
		)
		if err != nil || !outcome.Handled || !outcome.Delivered || notifyCalls != 1 {
			t.Fatalf("outcome=%+v err=%v notify_calls=%d", outcome, err, notifyCalls)
		}

		outcome, err = deliverTaskNotification(
			context.Background(),
			func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
				return core.TaskNotificationReceipt{}, errors.New("telegram down")
			},
			nil, taskID, userID, "ordinary", nil,
		)
		if err == nil || outcome.Handled || outcome.Delivered || !strings.Contains(err.Error(), "notify: telegram down") {
			t.Fatalf("failed ordinary notification outcome=%+v err=%v", outcome, err)
		}
	})

	t.Run("unkeyed no-op is handled without notify", func(t *testing.T) {
		beginCalls, notifyCalls := 0, 0
		journal := &schedulerNotificationJournal{begin: func(context.Context, uuid.UUID, uuid.UUID, string, []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
			beginCalls++
			return uuid.New(), true, nil
		}}
		outcome, err := deliverTaskNotification(
			context.Background(),
			func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
				notifyCalls++
				return receipt, nil
			},
			journal, taskID, userID, "[no-op]", nil,
		)
		if err != nil || !outcome.Handled || outcome.Delivered || notifyCalls != 0 || beginCalls != 0 {
			t.Fatalf("outcome=%+v err=%v notify=%d begin=%d", outcome, err, notifyCalls, beginCalls)
		}
	})

	t.Run("literal no-op inside ordinary text is delivered", func(t *testing.T) {
		notifyCalls := 0
		text := "model mentioned [no-op], but this is still a real notification"
		outcome, err := deliverTaskNotification(
			context.Background(),
			func(_ context.Context, _ uuid.UUID, gotText string) (core.TaskNotificationReceipt, error) {
				notifyCalls++
				if gotText != text {
					t.Fatalf("notify text = %q, want %q", gotText, text)
				}
				return receipt, nil
			},
			nil, taskID, userID, text, nil,
		)
		if err != nil || !outcome.Handled || !outcome.Delivered || notifyCalls != 1 {
			t.Fatalf("outcome=%+v err=%v notify=%d", outcome, err, notifyCalls)
		}
	})

	t.Run("keyed no-op fails closed", func(t *testing.T) {
		beginCalls, notifyCalls := 0, 0
		journal := &schedulerNotificationJournal{begin: func(context.Context, uuid.UUID, uuid.UUID, string, []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
			beginCalls++
			return uuid.New(), true, nil
		}}
		outcome, err := deliverTaskNotification(
			context.Background(),
			func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
				notifyCalls++
				return receipt, nil
			},
			journal, taskID, userID, "[no-op]", refs,
		)
		if err == nil || !outcome.Handled || outcome.Delivered || notifyCalls != 0 || beginCalls != 0 {
			t.Fatalf("outcome=%+v err=%v notify=%d begin=%d", outcome, err, notifyCalls, beginCalls)
		}
	})
}

func TestClassifyNotificationFailure(t *testing.T) {
	schedule := "5m"
	keyed := core.IterationResult{PendingDeliveries: []core.TaskDeliveryRef{{InputID: "notes", ItemKey: "note:1"}}}
	unkeyed := core.IterationResult{}
	ambiguous := errors.New("connection reset")

	tests := []struct {
		name         string
		task         core.AgentTask
		result       core.IterationResult
		outcome      taskNotificationOutcome
		err          error
		retryUnkeyed bool
		want         notificationFailureDisposition
	}{
		{name: "keyed one-shot admission failure never reruns task", task: core.AgentTask{}, result: keyed, err: errors.New("begin failed"), want: notificationFailureProceed},
		{name: "keyed recurring admission failure never reruns task", task: core.AgentTask{Schedule: &schedule}, result: keyed, outcome: taskNotificationOutcome{Handled: true}, err: errors.New("begin failed"), want: notificationFailureProceed},
		{name: "keyed retryable intent advances task", task: core.AgentTask{}, result: keyed, outcome: taskNotificationOutcome{Handled: true}, err: core.DefinitelyNotSent(errors.New("rate limited")), want: notificationFailureProceed},
		{name: "keyed permanent intent advances task", task: core.AgentTask{}, result: keyed, outcome: taskNotificationOutcome{Handled: true}, err: core.PermanentlyNotSent(errors.New("bot blocked")), want: notificationFailureProceed},
		{name: "legacy recurring done error still retries", task: core.AgentTask{Schedule: &schedule}, result: unkeyed, err: ambiguous, retryUnkeyed: true, want: notificationFailureRetry},
		{name: "legacy recurring milestone preserves transition", task: core.AgentTask{Schedule: &schedule}, result: unkeyed, err: ambiguous, want: notificationFailureProceed},
		{name: "unkeyed one-shot preserves legacy completion", task: core.AgentTask{}, result: unkeyed, err: errors.New("transport down"), retryUnkeyed: true, want: notificationFailureProceed},
		{name: "ambiguous keyed is handled but not delivered", task: core.AgentTask{}, result: keyed, outcome: taskNotificationOutcome{Handled: true}, err: ambiguous, want: notificationFailureProceed},
		{name: "confirm failure is handled and delivered", task: core.AgentTask{}, result: keyed, outcome: taskNotificationOutcome{Handled: true, Delivered: true}, err: errors.New("confirm failed"), want: notificationFailureProceed},
		{name: "success", task: core.AgentTask{}, result: keyed, outcome: taskNotificationOutcome{Handled: true, Delivered: true}, want: notificationFailureProceed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyNotificationFailure(test.task, test.result, test.outcome, test.err, test.retryUnkeyed); got != test.want {
				t.Fatalf("classification = %d, want %d", got, test.want)
			}
		})
	}
}

func TestDrainRetryableNotificationsUsesImmutableIntent(t *testing.T) {
	taskID, userID, soulID, attemptID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	refs := []core.TaskDeliveryRef{{InputID: "calendar", ItemKey: "event:shaorma"}}
	const immutableText = "Завтра закажите шаурму прямо в офисе."
	receipt := core.TaskNotificationReceipt{Transport: "telegram", BotID: "bot-1", ChatID: "42", MessageID: "100"}

	var stored *core.TaskNotificationIntent
	claimCalls, confirmCalls, handlerRuns := 0, 0, 0
	journal := &schedulerNotificationJournal{
		begin: func(_ context.Context, gotTaskID, gotUserID uuid.UUID, text string, gotRefs []core.TaskDeliveryRef) (uuid.UUID, bool, error) {
			if gotTaskID != taskID || gotUserID != userID || text != immutableText {
				t.Fatalf("begin task=%s user=%s text=%q", gotTaskID, gotUserID, text)
			}
			stored = &core.TaskNotificationIntent{ID: attemptID, TaskID: taskID, UserID: userID, Text: text, Refs: append([]core.TaskDeliveryRef(nil), gotRefs...)}
			return attemptID, true, nil
		},
		deferAttempt: func(_ context.Context, gotID uuid.UUID, reason string, retryAt time.Time) error {
			if gotID != attemptID || !strings.Contains(reason, "429") {
				t.Fatalf("defer id=%s reason=%q", gotID, reason)
			}
			if time.Until(retryAt) < defaultNotificationRetryDelay-time.Second {
				t.Fatalf("retry delay too short: retry_at=%s", retryAt)
			}
			return nil
		},
		claim: func(_ context.Context, _ time.Time) (*core.TaskNotificationIntent, error) {
			claimCalls++
			if claimCalls == 1 {
				copyIntent := *stored
				return &copyIntent, nil
			}
			return nil, nil
		},
		confirm: func(_ context.Context, gotID uuid.UUID, gotReceipt core.TaskNotificationReceipt) error {
			confirmCalls++
			if gotID != attemptID || gotReceipt != receipt {
				t.Fatalf("confirm id=%s receipt=%+v", gotID, gotReceipt)
			}
			return nil
		},
	}

	initialOutcome, initialErr := deliverTaskNotification(
		context.Background(),
		func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
			return core.TaskNotificationReceipt{}, core.DefinitelyNotSentAfter(errors.New("telegram 429"), 5*time.Second)
		},
		journal, taskID, userID, immutableText, refs,
	)
	if initialErr == nil || !initialOutcome.Handled || initialOutcome.Delivered || stored == nil {
		t.Fatalf("initial outcome=%+v err=%v stored=%+v", initialOutcome, initialErr, stored)
	}

	hookResult := make(chan core.IterationResult, 1)
	s := &Scheduler{
		handlers: map[string]core.AgentHandler{
			"heartbeat": schedulerHandlerFunc(func(context.Context, core.AgentTask, core.AgentDeps) (core.IterationResult, error) {
				handlerRuns++
				return core.IterationResult{}, nil
			}),
		},
		notifyJournal: journal,
		notifyTask: func(_ context.Context, gotTaskID uuid.UUID) (core.AgentTask, error) {
			if gotTaskID != taskID {
				t.Fatalf("lookup task=%s, want %s", gotTaskID, taskID)
			}
			return core.AgentTask{ID: taskID, UserID: userID, SoulID: soulID, Handler: "heartbeat"}, nil
		},
		notify: func(ctx context.Context, gotUserID uuid.UUID, text string) (core.TaskNotificationReceipt, error) {
			if gotUserID != userID || text != immutableText {
				t.Fatalf("retry user=%s text=%q; want immutable journal intent", gotUserID, text)
			}
			if !core.SingleAttemptNotificationFromContext(ctx) || core.SoulIDFromContext(ctx) != soulID || core.UserIDFromContext(ctx) != userID {
				t.Fatalf("retry context single=%v soul=%s user=%s", core.SingleAttemptNotificationFromContext(ctx), core.SoulIDFromContext(ctx), core.UserIDFromContext(ctx))
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) < 9*time.Second {
				t.Fatalf("retry transport deadline=%s, want fresh ~10s", deadline)
			}
			return receipt, nil
		},
		deps: &core.Deps{AgentIterationCompletedHook: func(_ context.Context, _ core.AgentTask, result core.IterationResult) {
			hookResult <- result
		}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s.drainRetryableNotifications(context.Background())

	if claimCalls != 2 || confirmCalls != 1 || handlerRuns != 0 {
		t.Fatalf("claim=%d confirm=%d handler_runs=%d; retry must not rerun task", claimCalls, confirmCalls, handlerRuns)
	}
	select {
	case result := <-hookResult:
		if !result.Notified || len(result.PendingDeliveries) != 1 || result.PendingDeliveries[0] != refs[0] {
			t.Fatalf("delivery hook result=%+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("deferred confirm did not fire delivery hook")
	}
}

func TestDrainRetryableNotificationsDefersPreflightFailuresAndIsBounded(t *testing.T) {
	userID := uuid.New()
	claimCalls, deferCalls, notifyCalls := 0, 0, 0
	journal := &schedulerNotificationJournal{
		claim: func(_ context.Context, _ time.Time) (*core.TaskNotificationIntent, error) {
			claimCalls++
			return &core.TaskNotificationIntent{
				ID: uuid.New(), TaskID: uuid.New(), UserID: userID, Text: "stored",
				Refs: []core.TaskDeliveryRef{{InputID: "notes", ItemKey: fmt.Sprintf("note:%d", claimCalls)}},
			}, nil
		},
		deferAttempt: func(_ context.Context, _ uuid.UUID, reason string, retryAt time.Time) error {
			deferCalls++
			if !strings.Contains(reason, "lookup task") || time.Until(retryAt) < defaultNotificationRetryDelay-time.Second {
				t.Fatalf("reason=%q retry_at=%s", reason, retryAt)
			}
			return nil
		},
	}
	s := &Scheduler{
		notifyJournal: journal,
		notifyTask: func(context.Context, uuid.UUID) (core.AgentTask, error) {
			return core.AgentTask{}, errors.New("database unavailable")
		},
		notify: func(context.Context, uuid.UUID, string) (core.TaskNotificationReceipt, error) {
			notifyCalls++
			return core.TaskNotificationReceipt{}, nil
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s.drainRetryableNotifications(context.Background())
	if claimCalls != maxNotificationRetriesPerTick || deferCalls != maxNotificationRetriesPerTick || notifyCalls != 0 {
		t.Fatalf("claim=%d defer=%d notify=%d; want bounded safe preflight defers", claimCalls, deferCalls, notifyCalls)
	}
}

func TestRetryTaskNotificationMissingSenderDefers(t *testing.T) {
	taskID, userID, soulID, attemptID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	deferCalls, hookCalls := 0, 0
	journal := &schedulerNotificationJournal{
		deferAttempt: func(ctx context.Context, gotID uuid.UUID, reason string, retryAt time.Time) error {
			deferCalls++
			if gotID != attemptID || !strings.Contains(reason, "sender unavailable") {
				t.Fatalf("defer id=%s reason=%q", gotID, reason)
			}
			if core.SoulIDFromContext(ctx) != soulID || core.UserIDFromContext(ctx) != userID {
				t.Fatalf("defer context soul=%s user=%s", core.SoulIDFromContext(ctx), core.UserIDFromContext(ctx))
			}
			if time.Until(retryAt) < defaultNotificationRetryDelay-time.Second {
				t.Fatalf("retry_at=%s, want default delay", retryAt)
			}
			return nil
		},
	}
	s := &Scheduler{
		notifyJournal: journal,
		notifyTask: func(context.Context, uuid.UUID) (core.AgentTask, error) {
			return core.AgentTask{ID: taskID, UserID: userID, SoulID: soulID}, nil
		},
		deps: &core.Deps{AgentIterationCompletedHook: func(context.Context, core.AgentTask, core.IterationResult) {
			hookCalls++
		}},
	}
	err := s.retryTaskNotification(context.Background(), core.TaskNotificationIntent{
		ID: attemptID, TaskID: taskID, UserID: userID, Text: "stored",
		Refs: []core.TaskDeliveryRef{{InputID: "notes", ItemKey: "note:1"}},
	})
	if err == nil || !core.IsDefinitelyNotSent(err) || deferCalls != 1 || hookCalls != 0 {
		t.Fatalf("err=%v defer_calls=%d hook_calls=%d", err, deferCalls, hookCalls)
	}
}

func TestRegistryForTaskEnforcesHandlerCeiling(t *testing.T) {
	base := core.NewToolRegistry()
	base.Register("safe", "", json.RawMessage(`{}`), noopTool)
	base.Register("dangerous", "", json.RawMessage(`{}`), noopTool)

	registry := registryForTask(base, []string{"safe"}, []string{"safe", "dangerous"}, true)
	if !registry.Has("safe") || registry.Has("dangerous") || registry.Count() != 1 {
		t.Fatalf("task expanded handler ceiling: definitions=%v", registry.Definitions())
	}

	// Without a handler ceiling, the legacy task list remains the narrowing.
	registry = registryForTask(base, nil, []string{"dangerous"}, true)
	if registry.Has("safe") || !registry.Has("dangerous") || registry.Count() != 1 {
		t.Fatalf("legacy no-ceiling selection changed: definitions=%v", registry.Definitions())
	}
}

func TestComposeSoulTaskRegistryCallsMCPForRequestedToolAndPreservesSoul(t *testing.T) {
	soulID := uuid.New()
	source := &recordingMCPSource{tools: []core.MCPTool{{
		Name:        "mcp__github__list_issues",
		Description: "issues",
		Schema:      json.RawMessage(`{}`),
		Handler:     noopTool,
	}}}
	cfg := &core.Config{MCPSource: source}
	scheduler := &Scheduler{deps: &core.Deps{Config: cfg}}
	base := core.NewToolRegistry()
	base.Register("native", "", json.RawMessage(`{}`), noopTool)

	registry, err := scheduler.composeSoulTaskRegistry(context.Background(), core.AgentTask{SoulID: soulID}, base, []string{"mcp__github__list_issues"})
	if err != nil {
		t.Fatalf("composeSoulTaskRegistry: %v", err)
	}
	if len(source.seen) != 1 || source.seen[0] != soulID {
		t.Fatalf("MCP source souls = %v, want [%s]", source.seen, soulID)
	}
	if !registry.Has("mcp__github__list_issues") || registry.PeerForTool("mcp__github__list_issues") != "mcp" {
		t.Fatalf("MCP tool not registered as peer:mcp: %v", registry.Definitions())
	}
	if base.Has("mcp__github__list_issues") {
		t.Fatal("shared native registry was mutated")
	}

	// The host wildcard admits MCP as a class, then the exact task request
	// narrows it to this one concrete capability.
	registry = registryForTask(registry, []string{"native", "peer:mcp"}, []string{"mcp__github__list_issues"}, true)
	if registry.Count() != 1 || !registry.Has("mcp__github__list_issues") || registry.Has("native") {
		t.Fatalf("MCP wildcard/request intersection failed: %v", registry.Definitions())
	}
}

func TestComposeSoulTaskRegistryDoesNotWakeMCPForNativeOnlyTask(t *testing.T) {
	source := &recordingMCPSource{}
	cfg := &core.Config{MCPSource: source}
	scheduler := &Scheduler{deps: &core.Deps{Config: cfg}}
	base := core.NewToolRegistry()
	base.Register("notes_list", "", json.RawMessage(`{}`), noopTool)

	registry, err := scheduler.composeSoulTaskRegistry(context.Background(), core.AgentTask{SoulID: uuid.New()}, base, []string{"notes_list"})
	if err != nil {
		t.Fatalf("composeSoulTaskRegistry: %v", err)
	}
	if len(source.seen) != 0 {
		t.Fatalf("native-only task cold-connected MCP for souls %v", source.seen)
	}
	if registry != base {
		t.Fatal("native-only task should reuse its native registry when no policy is configured")
	}
}

func TestTaskProgramQuietHoursSuppressMCPDiscovery(t *testing.T) {
	program := core.TaskProgram{
		Schema:     core.TaskProgramSchemaV1,
		Activation: core.TaskProgramActivation{Mode: core.TaskProgramActivationAlways},
		Decision:   core.TaskProgramDecision{Mode: core.TaskProgramDecisionSelected, Tools: []string{"mcp__github__list_issues"}},
		QuietHours: &core.TaskProgramQuietHours{FromHour: 22, ToHour: 7},
	}
	config, _ := json.Marshal(map[string]any{"task_program": program})
	task := core.AgentTask{SoulID: uuid.New(), Config: config}
	if !taskProgramInQuietHours(context.Background(), task, &core.Config{Timezone: "UTC"}, time.Date(2026, 7, 22, 23, 0, 0, 0, time.UTC)) {
		t.Fatal("23:00 UTC should be inside wrapped quiet hours")
	}
	if taskProgramInQuietHours(context.Background(), task, &core.Config{Timezone: "UTC"}, time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("12:00 UTC should be outside wrapped quiet hours")
	}
}

func TestComposeSoulTaskRegistryPolicyErrorFailsClosed(t *testing.T) {
	soulID := uuid.New()
	cfg := &core.Config{}
	cfg.Gateway.ResolveSoulToolPolicy = func(context.Context, uuid.UUID) (map[string]bool, []string, error) {
		return nil, nil, errors.New("policy database down")
	}
	scheduler := &Scheduler{deps: &core.Deps{Config: cfg}}
	base := core.NewToolRegistry()
	base.Register("message_send", "", json.RawMessage(`{}`), noopTool)

	registry, err := scheduler.composeSoulTaskRegistry(context.Background(), core.AgentTask{SoulID: soulID}, base, []string{"message_send"})
	if err == nil || !strings.Contains(err.Error(), "policy database down") {
		t.Fatalf("policy error = %v, want explicit failure", err)
	}
	if registry.Count() != 0 {
		t.Fatalf("policy error did not fail closed: %v", registry.Definitions())
	}
}

func TestComposeSoulTaskRegistryStripsDisconnectedAndDisabledTools(t *testing.T) {
	soulID := uuid.New()
	var policySoul uuid.UUID
	cfg := &core.Config{ToolMeta: map[string]core.ToolMeta{
		"calendar_list": {Provider: "google_calendar"},
		"disabled":      {},
		"core_internal": {Core: true},
	}}
	cfg.Gateway.ResolveSoulToolPolicy = func(_ context.Context, gotSoul uuid.UUID) (map[string]bool, []string, error) {
		policySoul = gotSoul
		return map[string]bool{"disabled": false, "core_internal": false}, []string{"github"}, nil
	}
	scheduler := &Scheduler{deps: &core.Deps{Config: cfg}}
	base := core.NewToolRegistry()
	for _, name := range []string{"calendar_list", "disabled", "core_internal", "notes_list"} {
		base.Register(name, "", json.RawMessage(`{}`), noopTool)
	}

	registry, err := scheduler.composeSoulTaskRegistry(context.Background(), core.AgentTask{SoulID: soulID}, base, []string{"notes_list"})
	if err != nil {
		t.Fatalf("composeSoulTaskRegistry: %v", err)
	}
	if policySoul != soulID {
		t.Fatalf("policy soul = %s, want %s", policySoul, soulID)
	}
	if registry.Has("calendar_list") || registry.Has("disabled") {
		t.Fatalf("disconnected/disabled tools survived policy: %v", registry.Definitions())
	}
	if !registry.Has("notes_list") || !registry.Has("core_internal") {
		t.Fatalf("default-on/core tools were lost: %v", registry.Definitions())
	}
}

func TestDurationDueTolerance(t *testing.T) {
	interval := 5 * time.Minute
	if !durationDue(interval-500*time.Millisecond, interval) {
		t.Fatal("sub-second early scheduler tick should be due")
	}
	if durationDue(interval-time.Second-time.Nanosecond, interval) {
		t.Fatal("scheduler must not run more than one second early")
	}
	if !durationDue(interval, interval) {
		t.Fatal("exact interval must be due")
	}

	// The tolerance is clamped below very short intervals.
	if durationDue(0, 500*time.Millisecond) {
		t.Fatal("short interval became continuously due")
	}
}

// newDailyGateScheduler builds the minimal Scheduler the daily_at gate
// needs: deps with a Config (process tz = UTC, i.e. "server time" in
// these tests) plus an optional per-soul ResolveTimezone hook, and a
// logger. No store/handlers — the gate must decide skips without them.
func newDailyGateScheduler(resolveTZ func(ctx context.Context) string, logW io.Writer) *Scheduler {
	if logW == nil {
		logW = io.Discard
	}
	cfg := &core.Config{Timezone: "UTC"}
	cfg.Gateway.ResolveTimezone = resolveTZ
	return &Scheduler{
		deps:   &core.Deps{Config: cfg},
		logger: slog.New(slog.NewTextHandler(logW, nil)),
	}
}

func dailyTask(config string, lastRun *time.Time) core.AgentTask {
	sched := "30m"
	t := core.AgentTask{
		ID:        uuid.New(),
		Title:     "morning digest",
		Handler:   "digest",
		Schedule:  &sched,
		LastRunAt: lastRun,
	}
	if config != "" {
		t.Config = json.RawMessage(config)
	}
	return t
}

// TestDailyGateOpen covers the once-a-day window semantics on the
// server-tz fallback path (no ResolveTimezone hook → process tz = UTC).
// A false result is a pre-dispatch skip: Run() `continue`s before
// SetRunning, so no handler run, no iteration, no last_run_at write.
// A true result only means the gate defers to the existing schedule/
// cadence checks — which is also why every absent/malformed-config case
// must come back true regardless of clock or last run.
func TestDailyGateOpen(t *testing.T) {
	at := func(hour, min int) time.Time {
		return time.Date(2026, 7, 8, hour, min, 0, 0, time.UTC)
	}
	dispatched := at(9, 5) // what SetRunning persisted at the 09:05 dispatch
	const cfg9 = `{"daily_at": {"hour": 9}}`

	cases := []struct {
		name    string
		config  string
		lastRun *time.Time
		now     time.Time
		want    bool
	}{
		// Window + once-per-day flow.
		{"before window skips", cfg9, nil, at(8, 55), false},
		{"in window, not yet run today, dispatches", cfg9, nil, at(9, 5), true},
		{"in window but already dispatched today skips", cfg9, &dispatched, at(9, 35), false},
		{"next day in window dispatches again", cfg9, &dispatched, at(9, 5).AddDate(0, 0, 1), true},
		{"ran yesterday outside window still skips", cfg9, &dispatched, at(15, 0).AddDate(0, 0, 1), false},
		// Hour boundary: the window is the whole local hour.
		{"last minute of window dispatches", cfg9, nil, at(9, 59), true},
		{"top of the next hour skips", cfg9, nil, at(10, 0), false},
		// No daily_at → gate open, existing schedule/cadence behavior untouched.
		{"nil config leaves gate open", "", &dispatched, at(9, 35), true},
		{"empty config object leaves gate open", `{}`, &dispatched, at(3, 0), true},
		{"config without daily_at leaves gate open", `{"prompt":"heartbeat"}`, &dispatched, at(3, 0), true},
		// Malformed hour → treated as absent (gate open).
		{"hour 24 treated as absent", `{"daily_at":{"hour":24}}`, &dispatched, at(3, 0), true},
		{"negative hour treated as absent", `{"daily_at":{"hour":-1}}`, nil, at(3, 0), true},
		{"missing hour treated as absent", `{"daily_at":{}}`, nil, at(3, 0), true},
		// Midnight is a valid window, distinct from "hour missing".
		{"hour 0 gates on midnight", `{"daily_at":{"hour":0}}`, nil, at(0, 30), true},
		{"hour 0 skips outside midnight", `{"daily_at":{"hour":0}}`, nil, at(9, 30), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newDailyGateScheduler(nil, nil)
			task := dailyTask(tc.config, tc.lastRun)
			if got := s.dailyGateOpen(context.Background(), task, tc.now); got != tc.want {
				t.Errorf("dailyGateOpen(now=%s, lastRun=%v) = %v, want %v",
					tc.now.Format("2006-01-02 15:04"), tc.lastRun, got, tc.want)
			}
		})
	}
}

// TestDailyGateSurvivesRestart proves the once-per-day guard is derived
// from the persisted task row (last_run_at, written by SetRunning at
// dispatch), not from scheduler memory: a brand-new Scheduler — a daemon
// restart — must still skip the rest of the day and reopen tomorrow.
func TestDailyGateSurvivesRestart(t *testing.T) {
	at := func(hour, min int) time.Time {
		return time.Date(2026, 7, 8, hour, min, 0, 0, time.UTC)
	}
	task := dailyTask(`{"daily_at": {"hour": 9}}`, nil)

	s1 := newDailyGateScheduler(nil, nil)
	if !s1.dailyGateOpen(context.Background(), task, at(9, 5)) {
		t.Fatal("09:05 first tick of the day: gate must be open")
	}
	// The dispatch stamped last_run_at = NOW() via SetRunning; that is the
	// only state the guard may rely on.
	ts := at(9, 5)
	task.LastRunAt = &ts

	s2 := newDailyGateScheduler(nil, nil) // restart: fresh in-memory state
	if s2.dailyGateOpen(context.Background(), task, at(9, 35)) {
		t.Error("09:35 after restart: gate must stay closed for the rest of the day")
	}
	if !s2.dailyGateOpen(context.Background(), task, at(9, 5).AddDate(0, 0, 1)) {
		t.Error("next day 09:05 after restart: gate must reopen")
	}
}

// TestDailyGateOwnerTimezone pins the gate to the task OWNER's wall
// clock, resolved through the soul-pinned Gateway.ResolveTimezone hook
// (the same one the background handler uses for [current_datetime]).
// Owner at UTC+3, server at UTC: 06:30 server time IS the owner's 9
// o'clock; 09:30 server time is not.
func TestDailyGateOwnerTimezone(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Moscow"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	utc := func(hour, min int) time.Time {
		return time.Date(2026, 7, 8, hour, min, 0, 0, time.UTC)
	}
	task := dailyTask(`{"daily_at": {"hour": 9}}`, nil)
	task.SoulID = uuid.New()

	var hookSoul uuid.UUID
	s := newDailyGateScheduler(func(ctx context.Context) string {
		hookSoul = core.SoulIDFromContext(ctx)
		return "Europe/Moscow" // UTC+3, no DST
	}, nil)

	if !s.dailyGateOpen(context.Background(), task, utc(6, 30)) {
		t.Error("06:30 UTC = 09:30 owner-local: gate must be open")
	}
	if hookSoul != task.SoulID {
		t.Errorf("ResolveTimezone saw soul %s, want the task's soul %s", hookSoul, task.SoulID)
	}
	if s.dailyGateOpen(context.Background(), task, utc(9, 30)) {
		t.Error("09:30 UTC = 12:30 owner-local: gate must be closed (server hour must not win)")
	}

	// Once-per-day compares owner-LOCAL dates: a run at 06:05 UTC (09:05
	// owner-local, same local day) closes the 06:30 UTC tick.
	ran := utc(6, 5)
	task.LastRunAt = &ran
	if s.dailyGateOpen(context.Background(), task, utc(6, 30)) {
		t.Error("06:30 UTC after a 06:05 UTC dispatch: same owner-local day, gate must be closed")
	}
	// 21:30 UTC July 8 = 00:30 July 9 owner-local — a NEW local day, but
	// outside the 9 o'clock window, so still closed.
	if s.dailyGateOpen(context.Background(), task, utc(21, 30)) {
		t.Error("21:30 UTC: new owner-local day but outside the window, gate must be closed")
	}
}

// TestDailyGateServerFallback: without a ResolveTimezone hook the gate
// documented fallback is the configured process (server) timezone.
func TestDailyGateServerFallback(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Moscow"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	cfg := &core.Config{Timezone: "Europe/Moscow"} // server at UTC+3, no hook
	s := &Scheduler{
		deps:   &core.Deps{Config: cfg},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	task := dailyTask(`{"daily_at": {"hour": 9}}`, nil)

	sixThirtyUTC := time.Date(2026, 7, 8, 6, 30, 0, 0, time.UTC) // 09:30 server-local
	if !s.dailyGateOpen(context.Background(), task, sixThirtyUTC) {
		t.Error("06:30 UTC = 09:30 server-local: gate must be open on the server-tz fallback")
	}
	nineThirtyUTC := time.Date(2026, 7, 8, 9, 30, 0, 0, time.UTC) // 12:30 server-local
	if s.dailyGateOpen(context.Background(), task, nineThirtyUTC) {
		t.Error("09:30 UTC = 12:30 server-local: gate must be closed on the server-tz fallback")
	}
}

// TestDailyAtMalformedWarnsOnce: a bad hour is re-parsed every tick, but
// the WARN must fire once per task, not every 60 seconds.
func TestDailyAtMalformedWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	s := newDailyGateScheduler(nil, &buf)
	task := dailyTask(`{"daily_at": {"hour": 24}}`, nil)

	for i := 0; i < 3; i++ {
		if _, ok := s.dailyAtHour(task); ok {
			t.Fatal("malformed hour must parse as absent")
		}
	}
	if n := strings.Count(buf.String(), "invalid daily_at hour"); n != 1 {
		t.Fatalf("want exactly 1 warning after 3 parses, got %d:\n%s", n, buf.String())
	}

	// A different task with its own bad config still gets its one warning.
	other := dailyTask(`{"daily_at": {}}`, nil)
	if _, ok := s.dailyAtHour(other); ok {
		t.Fatal("missing hour must parse as absent")
	}
	if n := strings.Count(buf.String(), "invalid daily_at hour"); n != 2 {
		t.Fatalf("want a second warning for a second task, got %d total", n)
	}
}
