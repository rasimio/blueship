package gateway

import (
	"context"
	"testing"

	"github.com/google/uuid"
	bs "github.com/rasimio/blueship/internal/core"
)

func TestHostCallbackRefreshesSameMessageWithResolvedScope(t *testing.T) {
	f := newFakeBotAPI(t)
	var got bs.BotCommandRequest
	g, bi := menuGateway(t, f, func(_ context.Context, in bs.BotCommandRequest) (bs.BotCommandResult, error) {
		got = in
		return bs.BotCommandResult{Text: "Running", Buttons: []bs.BotCommandButton{{Label: "Refresh", Command: "status", Args: in.Args}}}, nil
	})
	g.deps.Config.Gateway.Commands = append(g.deps.Config.Gateway.Commands, bs.BotCommand{Name: "status", Host: true})
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	g.users[telegramUserCacheKey(bi.id, tgCanonical(777))] = us
	cq := menuTap(hostCallbackPrefix + "status:abcd1234")
	cq.Message.Chat.ID = 777
	if !g.maybeHandleHostCallback(context.Background(), bi, cq) {
		t.Fatal("not handled")
	}
	if got.UserID != us.UserID || got.SoulID != us.SoulID || got.Args != "abcd1234" || got.BotID != bi.id {
		t.Fatalf("wrong scope: %+v", got)
	}
	calls := f.methods()
	if len(calls) != 1 || calls[0] != "editMessageText" {
		t.Fatalf("refresh sent extra messages: %v", calls)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls[0].body["message_id"] != float64(cq.Message.MessageID) {
		t.Fatal("edited wrong message")
	}
}

func TestHostCallbackRejectsGroupAndUndeclaredCommand(t *testing.T) {
	f := newFakeBotAPI(t)
	calls := 0
	g, bi := menuGateway(t, f, func(context.Context, bs.BotCommandRequest) (bs.BotCommandResult, error) {
		calls++
		return bs.BotCommandResult{}, nil
	})
	cq := menuTap(hostCallbackPrefix + "plus:")
	cq.Message.Chat.ID = -123
	g.maybeHandleHostCallback(context.Background(), bi, cq)
	cq.Message.Chat.ID = cq.From.ID
	cq.Data = hostCallbackPrefix + "not_registered:"
	g.maybeHandleHostCallback(context.Background(), bi, cq)
	if calls != 0 || len(f.methods()) != 0 {
		t.Fatal("untrusted callback reached handler")
	}
}
