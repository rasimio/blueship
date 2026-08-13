package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// fakeBotAPI records the calls a menu makes, so the assertions are about
// what Telegram is actually told rather than about an internal shape.
type fakeBotAPI struct {
	mu    sync.Mutex
	calls []apiCall
	srv   *httptest.Server
}

type apiCall struct {
	method string
	body   map[string]any
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		f.mu.Lock()
		f.calls = append(f.calls, apiCall{method: parts[len(parts)-1], body: body})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":501}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBotAPI) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.method)
	}
	return out
}

func (f *fakeBotAPI) last(method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].method == method {
			return f.calls[i].body
		}
	}
	return nil
}

// buttons flattens the inline keyboard of a recorded call to
// label → callback data (or url).
func buttons(body map[string]any) map[string]string {
	out := map[string]string{}
	markup, _ := body["reply_markup"].(map[string]any)
	rows, _ := markup["inline_keyboard"].([]any)
	for _, row := range rows {
		for _, b := range row.([]any) {
			btn := b.(map[string]any)
			target, _ := btn["callback_data"].(string)
			if target == "" {
				target, _ = btn["url"].(string)
			}
			out[btn["text"].(string)] = target
		}
	}
	return out
}

var testMenu = bs.BotMenu{
	Root:       "main",
	BackLabel:  "‹ Назад",
	CloseLabel: "Закрыть",
	Nodes: map[string]bs.BotMenuNode{
		"main": {Text: "Что показать?", Items: []bs.BotMenuItem{
			{Label: "Подписка", Node: "sub"},
			{Label: "Сайт", URL: "https://vaelum.ai"},
		}},
		"sub": {Text: "Подписка", Parent: "main", Items: []bs.BotMenuItem{
			{Label: "Оплатить", Command: "plus"},
		}},
	},
}

func menuGateway(t *testing.T, f *fakeBotAPI, handler bs.BotCommandHandler) (*Gateway, *botInstance) {
	t.Helper()
	cfg := &bs.Config{}
	cfg.Gateway.Menu = testMenu
	cfg.Gateway.Commands = []bs.BotCommand{{Name: "plus", Description: "Подписка", Host: true}}
	g := &Gateway{
		deps:   &bs.Deps{Config: cfg, CommandHandler: handler, BotOnboarding: noopOnboarding{}},
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

func menuTap(data string) *telegram.CallbackQuery {
	cq := &telegram.CallbackQuery{Data: data, From: &telegram.User{ID: 777}}
	cq.Message = &telegram.Message{MessageID: 99}
	cq.Message.Chat.ID = 42
	return cq
}

// Navigating edits the one message. A menu that posts a bubble per tap
// leaves a trail of dead screens in the chat, each with live buttons.
func TestMenuNavigatesInPlace(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := menuGateway(t, f, nil)

	if !g.openMenu(context.Background(), bi, 42) {
		t.Fatal("openMenu = false")
	}
	root := f.last("sendMessage")
	if root["text"] != "Что показать?" {
		t.Errorf("opened on %q, want the root screen", root["text"])
	}
	// A top screen has nowhere to go back to, so it must not pretend.
	if b := buttons(root); b["‹ Назад"] != "" {
		t.Error("the root screen offers a back button")
	} else if b["Закрыть"] != menuCallbackClose {
		t.Errorf("no close control on the root screen: %v", b)
	} else if b["Сайт"] != "https://vaelum.ai" {
		t.Errorf("link button = %q", b["Сайт"])
	}

	if !g.maybeHandleMenuCallback(context.Background(), bi, menuTap(menuCallbackNode+"sub")) {
		t.Fatal("the tap was not recognised as ours")
	}
	if got := f.methods(); got[len(got)-1] != "editMessageText" {
		t.Fatalf("navigation used %v, want an edit of the same message", got)
	}
	sub := f.last("editMessageText")
	if sub["text"] != "Подписка" {
		t.Errorf("edited to %q, want the child screen", sub["text"])
	}
	if b := buttons(sub); b["‹ Назад"] != menuCallbackNode+"main" {
		t.Errorf("no way back from a child screen: %v", b)
	}

	// And back, still in the same message.
	g.maybeHandleMenuCallback(context.Background(), bi, menuTap(menuCallbackNode+"main"))
	if f.last("editMessageText")["text"] != "Что показать?" {
		t.Error("back did not return to the root screen")
	}
	sends := 0
	for _, m := range f.methods() {
		if m == "sendMessage" {
			sends++
		}
	}
	if sends != 1 {
		t.Errorf("posted %d messages, want the menu to stay one", sends)
	}
}

// Close means gone. Stripping the keyboard instead would leave the menu's
// text scrolling through the chat forever with no way to remove it.
func TestMenuCloseDeletesTheMessage(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := menuGateway(t, f, nil)

	if !g.maybeHandleMenuCallback(context.Background(), bi, menuTap(menuCallbackClose)) {
		t.Fatal("close was not recognised")
	}
	if got := f.methods(); len(got) != 1 || got[0] != "deleteMessage" {
		t.Fatalf("close called %v, want deleteMessage", got)
	}
}

// A menu button must be able to carry any command the bot has, so it is
// replayed as though it had been typed rather than handed to one
// dispatcher. Routing it straight to the host handler would leave
// /persona and /reset as buttons that do nothing.
func TestMenuCommandIsReplayedAsATypedMessage(t *testing.T) {
	cq := menuTap(menuCallbackCommand + "persona")
	up := menuCommandUpdate(cq, "persona")

	if up.Message == nil {
		t.Fatal("no message synthesized")
	}
	if up.Message.Text != "/persona" {
		t.Errorf("text = %q, want /persona", up.Message.Text)
	}
	if up.Message.Chat.ID != cq.Message.Chat.ID {
		t.Error("replayed into a different chat")
	}
	if up.Message.From != cq.From {
		t.Error("replayed as somebody else")
	}
	// A name stored with its slash must not become "//persona", which is
	// not a command at all.
	if got := menuCommandUpdate(cq, "/plus").Message.Text; got != "/plus" {
		t.Errorf("text = %q, want /plus", got)
	}
}

// A command button both runs its command and takes the menu away —
// menu first, so the answer does not arrive under a menu the reader then
// has to dismiss.
func TestMenuCommandRunsAndClosesFirst(t *testing.T) {
	f := newFakeBotAPI(t)
	var got bs.BotCommandRequest
	g, bi := menuGateway(t, f, func(_ context.Context, in bs.BotCommandRequest) (bs.BotCommandResult, error) {
		got = in
		return bs.BotCommandResult{Text: "готово"}, nil
	})
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	g.users[telegramUserCacheKey(bi.id, tgCanonical(42))] = us

	if !g.maybeHandleMenuCallback(context.Background(), bi, menuTap(menuCallbackCommand+"plus")) {
		t.Fatal("the command tap was not recognised")
	}
	if got.Name != "plus" {
		t.Fatalf("handler saw %q, want plus", got.Name)
	}
	if got.UserID != us.UserID || got.SoulID != us.SoulID {
		t.Error("the command ran as the wrong person")
	}
	if calls := f.methods(); len(calls) < 2 || calls[0] != "deleteMessage" {
		t.Errorf("calls were %v, want the menu removed before the answer", calls)
	}
}

// Other flows own callbacks of their own; the menu must not eat them.
func TestMenuLeavesOtherCallbacksAlone(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := menuGateway(t, f, nil)
	for _, data := range []string{"vc:1", "tr:x", "sd:y", "hc:plus", "model_role:cortex"} {
		if g.maybeHandleMenuCallback(context.Background(), bi, menuTap(data)) {
			t.Errorf("the menu swallowed %q", data)
		}
	}
	if len(f.methods()) != 0 {
		t.Errorf("the menu called Telegram for someone else's callback: %v", f.methods())
	}
}

// A menu edited after a person opened one leaves live buttons pointing at
// screens that no longer exist. Removing it beats an inert keyboard.
func TestMenuRemovesAStaleScreen(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := menuGateway(t, f, nil)

	if !g.maybeHandleMenuCallback(context.Background(), bi, menuTap(menuCallbackNode+"gone")) {
		t.Fatal("the tap was not recognised")
	}
	if got := f.methods(); len(got) != 1 || got[0] != "deleteMessage" {
		t.Fatalf("stale tap called %v, want deleteMessage", got)
	}
}
