package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	bs "github.com/rasimio/blueship/internal/core"
)

func TestPinnedNotificationRouteChecksOwnershipWithoutPrimaryFallback(t *testing.T) {
	for _, mode := range []string{"valid", "foreign_user", "foreign_soul", "unpaired", "db_error", "removed_bot", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			user, soul, botID := uuid.New(), uuid.New(), uuid.New()
			bi := &botInstance{id: botID}
			calls := 0
			g := &Gateway{bots: map[uuid.UUID]*botInstance{botID: bi}, deps: &bs.Deps{}}
			g.deps.ResolveTelegramChat = func(_ context.Context, bot uuid.UUID, chat int64) (uuid.UUID, uuid.UUID, error) {
				calls++
				if bot != botID || chat != 42 {
					t.Error("route changed", bot, chat)
				}
				switch mode {
				case "foreign_user":
					return uuid.New(), soul, nil
				case "foreign_soul":
					return user, uuid.New(), nil
				case "unpaired":
					return uuid.Nil, uuid.Nil, bs.ErrTelegramChatUnpaired
				case "db_error":
					return uuid.Nil, uuid.Nil, errors.New("database unavailable")
				}
				return user, soul, nil
			}
			origin := bs.TaskOrigin{Transport: "telegram", BotID: botID, ChatID: "42"}
			if mode == "removed_bot" {
				delete(g.bots, botID)
			}
			if mode == "malformed" {
				origin.ChatID = "bad"
			}
			ctx := bs.ContextWithTaskOrigin(bs.WithSoulID(context.Background(), soul), origin)
			got, chat, err := g.resolveNotificationBot(ctx, user)
			if mode == "valid" {
				if err != nil || got != bi || chat != 42 || calls != 1 {
					t.Fatal(got, chat, err, calls)
				}
			} else {
				if err == nil || got != nil || isRetryableUserBotResolution(err) != (mode == "db_error") {
					t.Fatal(got, err)
				}
			}
		})
	}
}
