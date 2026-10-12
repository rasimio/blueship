package gateway

import (
	"context"
	"testing"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

type touch struct {
	bot  uuid.UUID
	chat int64
}

// Every message a person writes counts as using the bot, not only the
// first one after a restart. Recording it inside the resolver meant a
// cached conversation never recorded it again, and a host routing
// proactive messages by recent use sent them to whichever bot was
// touched last by anything — a guest summons, say.
func TestEveryInboundMessageRecordsBotUse(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := menuGateway(t, f, nil)
	var touched []touch
	g.deps.TouchTelegramChat = func(_ context.Context, botID uuid.UUID, chatID int64) error {
		touched = append(touched, touch{botID, chatID})
		return nil
	}
	g.deps.AuthorizeExecution = func(context.Context, bs.ExecutionRequest) (bs.ExecutionDecision, error) {
		return bs.ExecutionDecision{Allowed: false, Reason: "quota_exhausted"}, nil
	}
	g.users[telegramUserCacheKey(bi.id, tgCanonical(777))] = &UserState{UserID: uuid.New(), SoulID: uuid.New()}

	for range 2 {
		msg := &telegram.Message{From: &telegram.User{ID: 777}}
		msg.Chat.ID, msg.Chat.Type = 777, "private"
		g.prepareTelegramInbound(context.Background(), bi, msg, "hi")
	}
	if len(touched) != 2 || touched[0] != (touch{bi.id, 777}) || touched[1] != (touch{bi.id, 777}) {
		t.Fatalf("touched = %v, want the bot recorded for both messages", touched)
	}
}

// Calling a bot into another chat is not talking to it: the owner who
// summons the platform bot once must keep hearing from their own bot.
func TestGuestSummonsDoesNotRecordBotUse(t *testing.T) {
	r := &guestRouting{userID: uuid.New(), soulID: uuid.New(), decision: bs.ExecutionDecision{Allowed: true}}
	g, f := guestGateway(t, r)
	touched := 0
	g.deps.TouchTelegramChat = func(context.Context, uuid.UUID, int64) error {
		touched++
		return nil
	}

	if _, _, ok := g.admitGuest(context.Background(), guestBot(f, "platform", uuid.Nil), summons("@SoulBot hi")); !ok {
		t.Fatal("summons was not admitted")
	}
	if touched != 0 {
		t.Fatalf("a guest summons recorded bot use %d times", touched)
	}
}
