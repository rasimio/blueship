package gateway

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

func hostCommandGateway(commands []bs.BotCommand, handler bs.BotCommandHandler) *Gateway {
	cfg := &bs.Config{}
	cfg.Gateway.Commands = commands
	return &Gateway{
		deps:   &bs.Deps{Config: cfg, CommandHandler: handler},
		logger: slog.Default(),
		users:  map[string]*UserState{},
	}
}

var payMenu = []bs.BotCommand{
	{Name: "plus", Description: "Subscribe", Host: true},
	{Name: "help", Description: "What I can do", Prompt: "What can you do?"},
	{Name: "persona", Description: "Name and character"},
}

// The handler must see the command name and everything after it, so a
// single message can carry both the intent and the value it needs — the
// alternative is remembering that a question is outstanding.
func TestHostCommandPassesNameAndArgs(t *testing.T) {
	var got bs.BotCommandRequest
	g := hostCommandGateway(payMenu, func(_ context.Context, in bs.BotCommandRequest) (bs.BotCommandResult, error) {
		got = in
		return bs.BotCommandResult{}, nil
	})
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	bi := &botInstance{tgUsername: "TestBot"}

	if !g.maybeRunHostCommand(context.Background(), bi, 1, us, "/plus me@example.com") {
		t.Fatal("maybeRunHostCommand = false, want the command consumed")
	}
	if got.Name != "plus" {
		t.Errorf("name = %q, want plus", got.Name)
	}
	if got.Args != "me@example.com" {
		t.Errorf("args = %q, want the address", got.Args)
	}
	if got.UserID != us.UserID || got.SoulID != us.SoulID {
		t.Error("handler did not receive the caller's identity")
	}
}

// Everything else has to fall through untouched, or a host command list
// would start swallowing conversation and other commands' handlers.
func TestHostCommandIgnoresEverythingElse(t *testing.T) {
	called := false
	g := hostCommandGateway(payMenu, func(context.Context, bs.BotCommandRequest) (bs.BotCommandResult, error) {
		called = true
		return bs.BotCommandResult{}, nil
	})
	us := &UserState{UserID: uuid.New()}
	bi := &botInstance{tgUsername: "TestBot"}

	for name, in := range map[string]string{
		"prompt shortcut":  "/help",
		"frameworkcommand": "/reset",
		"menu entry":       "/persona",
		"unknown":          "/nonsense",
		"plain text":       "plus",
		"another bot":      "/plus@OtherBot me@example.com",
		"empty":            "",
	} {
		if g.maybeRunHostCommand(context.Background(), bi, 1, us, in) {
			t.Errorf("%s: %q was consumed as a host command", name, in)
		}
	}
	if called {
		t.Error("the handler ran for something that is not a host command")
	}
}

// A nil handler must not leave the command half-alive: the gateway has to
// fall through so the message reaches whatever would have handled it.
func TestHostCommandWithoutAHandlerFallsThrough(t *testing.T) {
	g := hostCommandGateway(payMenu, nil)
	us := &UserState{UserID: uuid.New()}
	if g.maybeRunHostCommand(context.Background(), &botInstance{}, 1, us, "/plus me@example.com") {
		t.Error("command consumed with no handler wired")
	}
}

// A failing handler still consumes the message. Falling through would
// hand "/plus me@example.com" to the model as though it were something
// somebody wanted to talk about.
func TestHostCommandConsumesOnHandlerError(t *testing.T) {
	g := hostCommandGateway(payMenu, func(context.Context, bs.BotCommandRequest) (bs.BotCommandResult, error) {
		return bs.BotCommandResult{}, context.DeadlineExceeded
	})
	us := &UserState{UserID: uuid.New()}
	if !g.maybeRunHostCommand(context.Background(), &botInstance{}, 1, us, "/plus me@example.com") {
		t.Error("a failed host command fell through to the model")
	}
}
