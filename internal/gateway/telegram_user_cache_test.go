package gateway

import (
	"testing"

	"github.com/google/uuid"
)

func TestTelegramUserCacheKeySeparatesBotsForSamePrivateChat(t *testing.T) {
	chatID := "telegram:123456"
	firstBot := uuid.New()
	secondBot := uuid.New()

	first := telegramUserCacheKey(firstBot, chatID)
	if first == telegramUserCacheKey(secondBot, chatID) {
		t.Fatal("the same Telegram private chat on different bots must have separate user states")
	}
	if first != telegramUserCacheKey(firstBot, chatID) {
		t.Fatal("cache key must be stable for one bot/chat pair")
	}
}
