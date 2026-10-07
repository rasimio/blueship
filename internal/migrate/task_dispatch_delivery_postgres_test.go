package migrate

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/agenttask"
	"github.com/rasimio/blueship/internal/core"
)

func TestPendingDeliveryDoesNotBlockGraphDispatch(t *testing.T) {
	db := finalizationDB(t)
	if _, err := db.Exec(`ALTER TABLE agent_tasks ADD COLUMN soul_id uuid DEFAULT gen_random_uuid(), ADD COLUMN cadence text, ADD COLUMN description text, ADD COLUMN acceptance_criteria text, ADD COLUMN delegate_to text, ADD COLUMN tools text[] NOT NULL DEFAULT '{}', ADD COLUMN use_agents text[] NOT NULL DEFAULT '{}', ADD COLUMN session_id text`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"026_agent_task_steps.sql", "027_agent_task_executor_version.sql", "028_agent_task_runs.sql", "031_agent_task_origin.sql"} {
		migration, err := migrations.ReadFile("sql/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	user, soul, bot := uuid.New(), uuid.New(), uuid.New()
	ctx := core.ContextWithTaskOrigin(core.WithSoulID(context.Background(), soul), core.TaskOrigin{Transport: "telegram", BotID: bot, ChatID: "42"})
	store := core.NewAgentTaskStore(db)
	create := func(title string, version int) core.AgentTask {
		t.Helper()
		task, err := store.Create(ctx, core.AgentTask{UserID: user, Title: title, Strategy: core.StrategyDirect, Handler: "background", ExecutorVersion: version})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	old := create("Previous completed task", 1)
	if _, changed, err := store.FinalizeTask(ctx, old.ID, core.TaskFinalization{Outcome: "completed", Body: "Previous report"}); err != nil || !changed {
		t.Fatal(changed, err)
	}
	fresh := create("New task", 2)
	entered, release, runFinished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	cfg := &core.Config{}
	cfg.ApplyDefaults()
	deps := &core.Deps{Config: cfg, EnsureAutonomousHistory: func(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }}
	scheduler := agenttask.NewScheduler(store, nil, nil, core.NewToolRegistry(), nil, deps, func(ctx context.Context, _ uuid.UUID, text string) (core.TaskNotificationReceipt, error) {
		if text == "Previous report" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return core.TaskNotificationReceipt{}, core.DefinitelyNotSent(ctx.Err())
			}
		}
		return core.TaskNotificationReceipt{Transport: "telegram", BotID: bot.String(), ChatID: "42", MessageID: "1"}, nil
	}, slog.New(slog.DiscardHandler))
	scheduler.SetGraphHandler(&schedulerGraphStub{})
	runResult := make(chan error, 1)
	go func() { defer close(runFinished); runResult <- scheduler.Run(ctx) }()
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); <-runFinished; scheduler.Wait() })
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("queued delivery did not start")
	}
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler dispatch waited for blocked transport")
	}
	reader := core.NewTaskStatusReader(db)
	until := time.Now().Add(2 * time.Second)
	for {
		rows, err := reader.Read(ctx, user, soul, fresh.ID.String())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].Outcome == "completed" {
			if rows[0].Result == nil || *rows[0].Result != "Verified report" || rows[0].DeliveryState != "pending" {
				t.Fatalf("result/delivery state conflated: %+v", rows[0])
			}
			break
		}
		if time.Now().After(until) {
			t.Fatal("new graph did not complete while old delivery was blocked")
		}
		time.Sleep(5 * time.Millisecond)
	}
	releaseOnce.Do(func() { close(release) })
	scheduler.Wait()
	for _, id := range []uuid.UUID{old.ID, fresh.ID} {
		rows, err := reader.Read(ctx, user, soul, id.String())
		if err != nil || len(rows) != 1 || rows[0].DeliveryState != "sent" || rows[0].Result == nil {
			t.Fatalf("delivery did not recover: %+v %v", rows, err)
		}
	}
}
