package gateway

import (
	"log/slog"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

func commandGateway(commands []bs.BotCommand) *Gateway {
	cfg := &bs.Config{}
	cfg.Gateway.Commands = commands
	return &Gateway{
		deps:   &bs.Deps{Config: cfg},
		logger: slog.Default(),
	}
}

var helpMenu = []bs.BotCommand{
	{Name: "help", Description: "What I can do", Prompt: "What can you do?"},
	{Name: "persona", Description: "Name and character"},
}

// The whole point of a prompt shortcut is that it becomes indistinguishable
// from the user typing the question, so the reply is a real turn rather
// than a static page.
func TestExpandCommandPromptRewritesConfiguredShortcuts(t *testing.T) {
	g := commandGateway(helpMenu)
	bi := &botInstance{tgUsername: "TestBot"}

	for name, in := range map[string]string{
		"bare":            "/help",
		"addressed":       "/help@TestBot",
		"uppercase":       "/HELP",
		"trailing args":   "/help про файлы",
		"leading spaces":  "  /help",
		"addressed+args":  "/help@TestBot про файлы",
		"mixed case addr": "/Help@testbot",
	} {
		if got := g.expandCommandPrompt(bi, in); got != "What can you do?" {
			t.Errorf("%s: expandCommandPrompt(%q) = %q, want the configured prompt", name, in, got)
		}
	}
}

// Everything else has to pass through byte-for-byte. A command with no
// Prompt is a menu entry for a handler that already exists — rewriting it
// would swallow the command before its handler ever saw it.
func TestExpandCommandPromptLeavesEverythingElseAlone(t *testing.T) {
	g := commandGateway(helpMenu)
	bi := &botInstance{tgUsername: "TestBot"}

	for name, in := range map[string]string{
		"menu entry without prompt": "/persona",
		"framework command":         "/start",
		"unknown command":           "/nonsense",
		"another bot":               "/help@SomeOtherBot",
		"plain text":                "what can you do?",
		"text with a slash inside":  "use the /help command",
		"empty":                     "",
	} {
		if got := g.expandCommandPrompt(bi, in); got != in {
			t.Errorf("%s: expandCommandPrompt(%q) = %q, want it unchanged", name, in, got)
		}
	}
}

func TestExpandCommandPromptWithNoConfiguredCommands(t *testing.T) {
	g := commandGateway(nil)
	bi := &botInstance{tgUsername: "TestBot"}
	if got := g.expandCommandPrompt(bi, "/help"); got != "/help" {
		t.Errorf("expandCommandPrompt = %q, want it unchanged when nothing is configured", got)
	}
}

// Telegram rejects the entire setMyCommands call when one entry is
// malformed. Dropping the bad row keeps the rest of the menu, which is
// the difference between a menu missing one line and no menu at all.
func TestTelegramCommandsDropsMalformedEntries(t *testing.T) {
	commands, skipped := telegramCommands([]bs.BotCommand{
		{Name: "help", Description: "What I can do"},
		{Name: "", Description: "No name"},
		{Name: "  ", Description: "Blank name"},
		{Name: "nodesc", Description: ""},
		{Name: "blankdesc", Description: "   "},
		{Name: "/reset", Description: "Start over"},
	})

	if len(commands) != 2 {
		t.Fatalf("kept %d commands, want 2: %+v", len(commands), commands)
	}
	if len(skipped) != 4 {
		t.Errorf("skipped = %v, want the four malformed entries", skipped)
	}
	// The leading slash is Telegram's own separator and must not be sent.
	if commands[1].Command != "reset" {
		t.Errorf("command = %q, want the leading slash stripped", commands[1].Command)
	}
	if commands[0].Command != "help" || commands[0].Description != "What I can do" {
		t.Errorf("first command = %+v", commands[0])
	}
}

// An empty menu is a legitimate configuration — it clears whatever was set
// before — and must not be conflated with "leave the old menu alone".
func TestTelegramCommandsOnEmptyConfigReturnsEmptyNotNil(t *testing.T) {
	commands, skipped := telegramCommands(nil)
	if commands == nil {
		t.Error("commands = nil; SetMyCommands must receive an empty list to clear the menu")
	}
	if len(commands) != 0 || len(skipped) != 0 {
		t.Errorf("commands = %+v, skipped = %v", commands, skipped)
	}
}
