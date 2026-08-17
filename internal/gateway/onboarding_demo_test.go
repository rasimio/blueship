package gateway

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

func demoGateway(t *testing.T, f *fakeBotAPI, buttons []bs.OnboardingSeedButton) (*Gateway, *botInstance) {
	t.Helper()
	cfg := &bs.Config{}
	cfg.Gateway.OnboardingFlow = bs.OnboardingFlow{
		Mode:        bs.OnboardingModeInstant,
		SeedButtons: buttons,
	}
	g := &Gateway{
		deps:   &bs.Deps{Config: cfg, BotOnboarding: noopOnboarding{}},
		logger: slog.New(slog.DiscardHandler),
		users:  map[string]*UserState{},
	}
	bi := &botInstance{
		id:         uuid.New(),
		tgUsername: "TestBot",
		client:     telegram.NewClientWithAPIURL("t", f.srv.URL, 5*time.Second),
	}
	return g, bi
}

func seedTap(index string) *telegram.CallbackQuery {
	cq := &telegram.CallbackQuery{Data: onbCallbackSeed + index, From: &telegram.User{ID: 777}}
	cq.Message = &telegram.Message{MessageID: 99}
	cq.Message.Chat.ID = 42
	return cq
}

// A demonstration button shows what the product produces without first
// making it true of the person looking. Dispatching its text as their
// own message would write an invented commitment into the memory of
// somebody who has said nothing yet — and the button exists precisely
// so they can look before that happens.
func TestOnboardingDemoButtonSaysNothingForThePerson(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := demoGateway(t, f, []bs.OnboardingSeedButton{
		{Label: "Показать пример", Reply: "Вот как выглядит запись."},
	})
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	g.users[telegramUserCacheKey(bi.id, tgCanonical(42))] = us

	if !g.onboardingHandleSeed(context.Background(), bi, seedTap("0"), "0") {
		t.Fatal("the tap was not handled")
	}
	// The inbound activity fence is the first thing a real turn touches,
	// before any model or transport call — so it is where a turn that
	// should not have started is still visible.
	if v := g.activityState(us.UserID, us.SoulID).Version; v != 0 {
		t.Errorf("a demonstration entered the turn machinery (activity version %d)", v)
	}

	sent := f.last("sendMessage")
	if sent == nil {
		t.Fatal("the demonstration showed nothing")
	}
	if sent["text"] != "Вот как выглядит запись." {
		t.Errorf("showed %q", sent["text"])
	}
	// The keyboard is spent either way: a demo card ends by asking for
	// the person's own line, and a live button under it invites a second
	// look at the same example instead.
	if f.last("editMessageReplyMarkup") == nil {
		t.Error("the keyboard survived the tap")
	}
}

// The seeding button must keep seeding: its text is dispatched as though
// the person typed it, so the reply is a real turn in their own history.
func TestOnboardingSeedButtonStillSpeaksAsThePerson(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := demoGateway(t, f, []bs.OnboardingSeedButton{
		{Label: "Проверить память", Text: "Расспроси меня о чём-нибудь важном."},
	})

	g.onboardingHandleSeed(context.Background(), bi, seedTap("0"), "0")

	// No user state and no handler, so the turn dies immediately — what
	// matters is that nothing was posted back as an answer, which is what
	// a Reply button would have done.
	for _, m := range f.methods() {
		if m == "sendMessage" {
			t.Fatalf("a seeding button answered instead of asking: %v", f.methods())
		}
	}
}

// A stale keyboard from before a config change must not index into the
// new list. Out of range is a tap on a button that no longer exists.
func TestOnboardingSeedIgnoresAStaleIndex(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := demoGateway(t, f, []bs.OnboardingSeedButton{
		{Label: "Показать пример", Reply: "Вот как выглядит запись."},
	})

	if !g.onboardingHandleSeed(context.Background(), bi, seedTap("7"), "7") {
		t.Fatal("a stale tap should still be consumed")
	}
	if len(f.methods()) != 0 {
		t.Errorf("a stale tap did something: %v", f.methods())
	}
}
