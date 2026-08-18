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

// A tap on the persistent keyboard arrives as its own label — that kind
// of keyboard has no callbacks. Unless the transport turns it back into
// a command, «Подписка» reaches the model as something the person said,
// and gets answered with a sentence about subscriptions instead of the
// checkout.
func TestKeyboardTapReachesTheCommandItStandsFor(t *testing.T) {
	f := newFakeBotAPI(t)
	cfg := &bs.Config{}
	cfg.Gateway.Commands = []bs.BotCommand{{Name: "plus", Description: "Подписка", Host: true}}
	cfg.Gateway.Keyboard = bs.BotKeyboard{
		Root: "main", CloseLabel: "Закрыть", Closed: "Закрыла.",
		Nodes: map[string]bs.BotKeyboardNode{
			"main": {Text: "Меню", Rows: [][]bs.BotKeyboardKey{
				{{Label: "Подписка", Command: "plus"}},
			}},
		},
	}

	var got bs.BotCommandRequest
	g := &Gateway{
		deps: &bs.Deps{Config: cfg, BotOnboarding: noopOnboarding{},
			CommandHandler: func(_ context.Context, in bs.BotCommandRequest) (bs.BotCommandResult, error) {
				got = in
				return bs.BotCommandResult{Text: "ok"}, nil
			}},
		logger: slog.New(slog.DiscardHandler),
		users:  map[string]*UserState{},
	}
	bi := &botInstance{id: uuid.New(), tgUsername: "TestBot",
		client: telegram.NewClientWithAPIURL("t", f.srv.URL, 5*time.Second)}
	g.users[telegramUserCacheKey(bi.id, tgCanonical(42))] = &UserState{
		UserID: uuid.New(), SoulID: uuid.New()}

	send := func(text string) {
		msg := &telegram.Message{MessageID: 1, From: &telegram.User{ID: 777}, Text: text}
		msg.Chat = telegram.Chat{ID: 42, Type: "private"}
		g.handleUpdate(context.Background(), bi, telegram.Update{Message: msg})
	}

	send("Подписка")
	if got.Name != "plus" {
		t.Fatalf("the tap reached %q, want the plus command", got.Name)
	}

	// Precision — that a question merely containing the label is left
	// alone — is covered by TestKeyboardMapsOnlyTheExactLabel. It cannot
	// be checked here: anything that is not a command goes on to the
	// model path, which needs the whole gateway standing up.
}

// /menu has to be answered by the transport whichever kind of menu the
// host configured. Gating on the inline one alone left a bot that
// advertised /menu in Telegram's own command list and then let it fall
// through to the model, which said no such command existed.
func TestMenuCommandIsAnsweredByAKeyboardAlone(t *testing.T) {
	f := newFakeBotAPI(t)
	cfg := &bs.Config{}
	cfg.Gateway.Commands = []bs.BotCommand{{Name: "menu", Description: "Меню", Menu: true}}
	cfg.Gateway.Keyboard = bs.BotKeyboard{
		Root: "main", CloseLabel: "Закрыть", Closed: "Убрала.",
		Nodes: map[string]bs.BotKeyboardNode{
			"main": {Text: "Меню.", Rows: [][]bs.BotKeyboardKey{
				{{Label: "Умения", Say: "покажи, что умеешь"}},
			}},
		},
	}
	// No inline menu configured at all — the shape a host ships when it wants
	// only the persistent keyboard.
	g := &Gateway{
		deps:   &bs.Deps{Config: cfg, BotOnboarding: noopOnboarding{}},
		logger: slog.New(slog.DiscardHandler),
		users:  map[string]*UserState{},
	}
	bi := &botInstance{id: uuid.New(), tgUsername: "TestBot",
		client: telegram.NewClientWithAPIURL("t", f.srv.URL, 5*time.Second)}
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}

	if !g.maybeRunHostCommand(context.Background(), bi, 42, 777, us, "/menu") {
		t.Fatal("/menu fell through; the model would be asked to explain a command it does not have")
	}
	sent := f.last("sendMessage")
	if sent == nil {
		t.Fatal("nothing was sent")
	}
	markup, _ := sent["reply_markup"].(map[string]any)
	if _, ok := markup["keyboard"]; !ok {
		t.Errorf("/menu answered without the keyboard: %v", sent["reply_markup"])
	}
}
