package agenttask

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

func TestNotificationPropagatesPersistedOriginWithoutHostValidator(t *testing.T) {
	origin := core.TaskOrigin{Transport: "telegram", BotID: uuid.New(), ChatID: "42"}
	raw, _ := json.Marshal(map[string]any{"delivery_origin": origin})
	task := core.AgentTask{UserID: uuid.New(), SoulID: uuid.New(), Config: raw}
	calls := 0
	s := &Scheduler{notify: func(ctx context.Context, user uuid.UUID, text string) (core.TaskNotificationReceipt, error) {
		calls++
		got, pinned := core.TaskOriginFromContext(ctx)
		if !pinned || got != origin || user != task.UserID || core.SoulIDFromContext(ctx) != task.SoulID || text != "saved report" {
			t.Error("lost immutable notification route", got, pinned, user, text)
		}
		return core.TaskNotificationReceipt{Transport: "telegram", MessageID: "1"}, nil
	}}
	if _, err := s.validatedTaskNotifier(task, nil)(context.Background(), task.UserID, "saved report"); err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
	if _, err := s.validatedTaskNotifier(task, nil)(context.Background(), uuid.New(), "saved report"); err == nil || calls != 1 {
		t.Fatal("foreign user reached sender", calls, err)
	}
	task.Config = json.RawMessage(`{"delivery_origin":null}`)
	if _, err := s.validatedTaskNotifier(task, nil)(context.Background(), task.UserID, "saved report"); err == nil || calls != 1 {
		t.Fatal("malformed origin fell back", calls, err)
	}
}
