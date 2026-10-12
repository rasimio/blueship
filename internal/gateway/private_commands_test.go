package gateway

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

var privateMenu = []bs.BotCommand{
	{Name: "help", Description: "What I can do", Prompt: "What can you do?"},
	{Name: "wipe", Description: "Start over", Host: true, Private: true},
	{Name: "wipe_ok", Host: true, Exclusive: true},
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func callsOf(f *fakeBotAPI, method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c.body)
		}
	}
	return out
}

// The menu everyone gets must not name a Private command, and the menu of a
// chat that may see it must still carry everything else: Telegram replaces
// a chat's menu with the chat-scoped list rather than merging the two.
func TestPrivateCommandsStayOutOfTheSharedMenu(t *testing.T) {
	shared, _ := telegramCommands(privateMenu)
	for _, c := range shared {
		if c.Command == "wipe" || c.Command == "wipe_ok" {
			t.Fatalf("shared menu lists %q", c.Command)
		}
	}
	private := privateTelegramCommands(privateMenu)
	names := map[string]bool{}
	for _, c := range private {
		names[c.Command] = true
	}
	if !names["help"] || !names["wipe"] {
		t.Fatalf("private menu = %v, want help and wipe", private)
	}
	if names["wipe_ok"] {
		t.Fatal("private menu lists a host command with no description")
	}
}

func privateGateway(t *testing.T, f *fakeBotAPI, handler bs.BotCommandHandler, onb bs.BotOnboarding) (*Gateway, *botInstance) {
	t.Helper()
	cfg := &bs.Config{}
	cfg.Gateway.Commands = privateMenu
	cfg.Gateway.CommandAudience = func(c bs.TelegramCaller) bool { return c.Username == "owner" }
	cfg.UI.CommandBusy = "busy"
	if onb == nil {
		onb = noopOnboarding{}
	}
	g := &Gateway{
		deps:   &bs.Deps{Config: cfg, CommandHandler: handler, BotOnboarding: onb},
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

// Only the admitted person's own private chat gets the menu, and only once
// per process: it is Telegram that keeps it afterwards.
func TestPrivateMenuIsPublishedToTheAudienceOnly(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := privateGateway(t, f, nil, nil)
	ctx := context.Background()

	g.maybePublishPrivateCommands(ctx, bi, bs.TelegramCaller{UserID: 9, ChatID: 9, Username: "stranger"})
	g.maybePublishPrivateCommands(ctx, bi, bs.TelegramCaller{UserID: 7, ChatID: -100, Username: "owner"})
	g.maybePublishPrivateCommands(ctx, bi, bs.TelegramCaller{UserID: 7, ChatID: 7, Username: "owner"})
	eventually(t, "the private menu", func() bool { return len(callsOf(f, "setMyCommands")) == 1 })
	g.maybePublishPrivateCommands(ctx, bi, bs.TelegramCaller{UserID: 7, ChatID: 7, Username: "owner"})
	time.Sleep(50 * time.Millisecond)

	calls := callsOf(f, "setMyCommands")
	if len(calls) != 1 {
		t.Fatalf("setMyCommands sent %d times, want once", len(calls))
	}
	scope, _ := calls[0]["scope"].(map[string]any)
	if scope["type"] != "chat" || scope["chat_id"] != float64(7) {
		t.Fatalf("scope = %v, want the owner's chat", scope)
	}
	listed := map[string]bool{}
	for _, c := range calls[0]["commands"].([]any) {
		listed[c.(map[string]any)["command"].(string)] = true
	}
	if !listed["wipe"] || !listed["help"] {
		t.Fatalf("published %v, want the whole menu plus wipe", listed)
	}
}

// Who tapped comes from Telegram's update, never from the button: a
// handler deciding access on it must see the tapper, whatever the
// callback data says.
func TestHostCallbackReportsWhoTapped(t *testing.T) {
	f := newFakeBotAPI(t)
	var got bs.BotCommandRequest
	g, bi := privateGateway(t, f, func(_ context.Context, in bs.BotCommandRequest) (bs.BotCommandResult, error) {
		got = in
		return bs.BotCommandResult{Text: "ok"}, nil
	}, nil)
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	g.users[telegramUserCacheKey(bi.id, tgCanonical(777))] = us
	cq := menuTap(hostCallbackPrefix + "wipe:777")
	cq.From = &telegram.User{ID: 777, Username: "owner", FirstName: "Sofia"}
	cq.Message.Chat.ID = 777

	g.maybeHandleHostCallback(context.Background(), bi, cq)
	want := bs.TelegramCaller{UserID: 777, ChatID: 777, Username: "owner", FirstName: "Sofia"}
	if got.Caller != want {
		t.Fatalf("caller = %+v, want %+v", got.Caller, want)
	}
}

// restartOnboarding is an instant-mode hook that remembers the accounts it
// was asked to create.
type restartOnboarding struct {
	noopOnboarding
	mu      sync.Mutex
	created []bs.BotOnboardingComplete
	userID  uuid.UUID
	soulID  uuid.UUID
}

func (o *restartOnboarding) CompleteOnboarding(_ context.Context, in bs.BotOnboardingComplete) (uuid.UUID, uuid.UUID, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.created = append(o.created, in)
	return o.userID, o.soulID, nil
}

func (o *restartOnboarding) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.created)
}

// An Exclusive command runs with its conversation stopped and locked, and a
// Restart answer leaves no cached trace of the account on any bot before
// the chat is opened again the way a newcomer's first /start opens it.
func TestExclusiveRestartQuietsForgetsAndGreetsAsNew(t *testing.T) {
	f := newFakeBotAPI(t)
	onb := &restartOnboarding{userID: uuid.New(), soulID: uuid.New()}
	old := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	other := &UserState{UserID: uuid.New(), SoulID: uuid.New()}

	var lockHeld bool
	var g *Gateway
	g, bi := privateGateway(t, f, func(ctx context.Context, in bs.BotCommandRequest) (bs.BotCommandResult, error) {
		if in.Quiet == nil {
			t.Error("an Exclusive command got no way to quiet its conversation")
			return bs.BotCommandResult{}, nil
		}
		if err := in.Quiet(ctx); err != nil {
			return bs.BotCommandResult{}, err
		}
		// From here to the end of the handler the turn lock is held.
		mu := g.turnMutex(old.UserID, old.SoulID)
		if mu.TryLock() {
			mu.Unlock()
		} else {
			lockHeld = true
		}
		return bs.BotCommandResult{Text: "Reset done", Restart: true}, nil
	}, onb)
	g.deps.Config.Gateway.OnboardingFlow = bs.OnboardingFlow{
		Mode: bs.OnboardingModeInstant, DefaultName: "Mira", DefaultVoice: "warm", Welcome: "Hello, newcomer",
	}

	otherBot := uuid.New()
	g.users[telegramUserCacheKey(bi.id, tgCanonical(777))] = old
	g.users[telegramUserCacheKey(otherBot, tgCanonical(777))] = old
	g.users[telegramUserCacheKey(bi.id, tgCanonical(555))] = other
	g.setKeyboardScreen(bi.id, tgCanonical(777), "settings")

	turnCtx, _, done := g.beginTurn(context.Background(), old.UserID, old.SoulID)
	defer done()

	cq := menuTap(hostCallbackPrefix + "wipe_ok:token")
	cq.From = &telegram.User{ID: 777, Username: "owner", FirstName: "Sofia"}
	cq.Message.Chat.ID = 777
	if !g.maybeHandleHostCallback(context.Background(), bi, cq) {
		t.Fatal("callback not handled")
	}
	eventually(t, "the newcomer greeting", func() bool { return onb.count() == 1 && len(callsOf(f, "sendMessage")) == 1 })

	if turnCtx.Err() == nil {
		t.Error("the answer in flight was not stopped")
	}
	if !lockHeld {
		t.Error("the handler ran without the conversation's turn lock")
	}
	if g.cachedUserState(bi.id, tgCanonical(777)) != nil || g.cachedUserState(otherBot, tgCanonical(777)) != nil {
		t.Error("a chat of the reset account is still cached")
	}
	if g.cachedUserState(bi.id, tgCanonical(555)) != other {
		t.Error("someone else's chat was forgotten")
	}
	if g.keyboardScreen(bi.id, tgCanonical(777)) != "" {
		t.Error("the keyboard screen survived the reset")
	}
	if !g.conversationRetired(old.UserID, old.SoulID) {
		t.Error("turns queued for the reset account would still run")
	}
	if mu := g.turnMutex(old.UserID, old.SoulID); !mu.TryLock() {
		t.Error("the conversation was left locked")
	} else {
		mu.Unlock()
	}
	if edit := f.last("editMessageText"); edit == nil || edit["text"] != "Reset done" {
		t.Fatalf("the tapped message was not replaced by the answer: %v", edit)
	}
	if welcome := f.last("sendMessage"); welcome["text"] != "Hello, newcomer" {
		t.Fatalf("greeted with %v, want the instant-mode welcome", welcome["text"])
	}
	in := onb.created[0]
	if in.TGUserID != 777 || in.TGChatID != 777 || in.UserHandle != "owner" || in.UserDisplayName != "Sofia" {
		t.Fatalf("account created as %+v, want the tapper", in)
	}
	if us := g.cachedUserState(bi.id, tgCanonical(777)); us != nil && us.UserID == old.UserID {
		t.Fatal("the chat went back to the old account")
	}
}

// A conversation that will not quiet down gets a "busy" note and the
// handler never runs: half a reset beside a live turn is the thing
// Exclusive exists to prevent.
func TestExclusiveCommandGivesUpOnABusyConversation(t *testing.T) {
	prev := quietWait
	quietWait = 100 * time.Millisecond
	defer func() { quietWait = prev }()

	f := newFakeBotAPI(t)
	ran := false
	g, bi := privateGateway(t, f, func(ctx context.Context, in bs.BotCommandRequest) (bs.BotCommandResult, error) {
		if err := in.Quiet(ctx); err != nil {
			return bs.BotCommandResult{}, err
		}
		ran = true
		return bs.BotCommandResult{Text: "done", Restart: true}, nil
	}, nil)
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	g.users[telegramUserCacheKey(bi.id, tgCanonical(777))] = us
	mu := g.turnMutex(us.UserID, us.SoulID)
	mu.Lock()
	defer mu.Unlock()

	cq := menuTap(hostCallbackPrefix + "wipe_ok:token")
	cq.From = &telegram.User{ID: 777, Username: "owner"}
	cq.Message.Chat.ID = 777
	g.maybeHandleHostCallback(context.Background(), bi, cq)
	eventually(t, "the busy note", func() bool { return len(callsOf(f, "sendMessage")) == 1 })

	if ran {
		t.Fatal("the handler ran while a turn held the conversation")
	}
	if got := f.last("sendMessage")["text"]; got != "busy" {
		t.Fatalf("sent %v, want the busy note", got)
	}
	if callsOf(f, "editMessageText") != nil {
		t.Fatal("the tapped message lost its buttons although nothing happened")
	}
	if g.cachedUserState(bi.id, tgCanonical(777)) != us {
		t.Fatal("the chat was forgotten although nothing was reset")
	}
}

// A request the handler turns down — somebody not allowed, a button from an
// earlier walk-through — never asks for quiet, so the answer being written
// goes on as if nothing had been tapped.
func TestExclusiveCommandRefusedWithoutQuietLeavesTheAnswerRunning(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := privateGateway(t, f, func(context.Context, bs.BotCommandRequest) (bs.BotCommandResult, error) {
		return bs.BotCommandResult{Text: "out of date"}, nil
	}, nil)
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	g.users[telegramUserCacheKey(bi.id, tgCanonical(777))] = us
	turnCtx, _, done := g.beginTurn(context.Background(), us.UserID, us.SoulID)
	defer done()

	cq := menuTap(hostCallbackPrefix + "wipe_ok:old")
	cq.From = &telegram.User{ID: 777, Username: "owner"}
	cq.Message.Chat.ID = 777
	g.maybeHandleHostCallback(context.Background(), bi, cq)
	eventually(t, "the refusal", func() bool { return len(callsOf(f, "editMessageText")) == 1 })

	if turnCtx.Err() != nil {
		t.Fatal("a refused request stopped the answer in flight")
	}
	if g.conversationRetired(us.UserID, us.SoulID) || g.cachedUserState(bi.id, tgCanonical(777)) != us {
		t.Fatal("a refused request touched the conversation")
	}
}

// Someone taken off the list who still has the menu from before loses it the
// first time they use it; whoever is still on the list keeps it.
func TestPrivateMenuIsTakenBackFromWhoeverLostAccess(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := privateGateway(t, f, func(context.Context, bs.BotCommandRequest) (bs.BotCommandResult, error) {
		return bs.BotCommandResult{Text: "no such command"}, nil
	}, nil)
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}

	g.maybeRunHostCommand(context.Background(), bi, bs.TelegramCaller{UserID: 7, ChatID: 7, Username: "owner"}, us, "/wipe")
	g.maybeRunHostCommand(context.Background(), bi, bs.TelegramCaller{UserID: 9, ChatID: 9, Username: "former"}, us, "/wipe")
	eventually(t, "the menu retraction", func() bool { return len(callsOf(f, "deleteMyCommands")) == 1 })
	time.Sleep(50 * time.Millisecond)

	calls := callsOf(f, "deleteMyCommands")
	if len(calls) != 1 {
		t.Fatalf("deleteMyCommands sent %d times, want once", len(calls))
	}
	if scope, _ := calls[0]["scope"].(map[string]any); scope["chat_id"] != float64(9) {
		t.Fatalf("retracted %v, want the former owner's chat", scope)
	}
}

// A turn that waited out a reset on the conversation's lock is dropped
// before it reads or writes anything: the soul it would answer from is gone.
func TestTurnQueuedForAResetAccountIsDropped(t *testing.T) {
	f := newFakeBotAPI(t)
	g, _ := privateGateway(t, f, nil, nil)
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	g.retireConversation(us.UserID, us.SoulID)

	g.processMessages(context.Background(), us, []pendingMsg{{text: "hello"}}, nil)

	if _, running := g.ActiveTurn(us.UserID, us.SoulID); running {
		t.Fatal("a turn started for a reset account")
	}
	if len(f.methods()) != 0 {
		t.Fatalf("the dropped turn talked to Telegram: %v", f.methods())
	}
}
