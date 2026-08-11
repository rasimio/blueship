package gateway

import (
	"testing"

	"github.com/rasimio/blueship/internal/core"
)

// A deep link carries one thing: the /start payload. It is the only way
// one chat can hand a person to another — which a host needs when the
// bot they are in cannot complete what they came to do. An invoice is
// paid to the bot that created it, so a purchase started on somebody
// else's bot has to be finished on ours.
//
// The payload has to become the command, or the link drops the person
// into a silent chat with their wallet out and nothing happening.
func TestStartPayloadNamingAHostCommandRunsIt(t *testing.T) {
	g := gatewayWithCommands([]core.BotCommand{
		{Name: "plus", Host: true},
		{Name: "help"}, // not host-handled
	})

	for _, tc := range []struct {
		text     string
		wantName string
		wantRun  bool
	}{
		{"/start plus", "plus", true},
		{"/start PLUS", "plus", true},
		// Attribution tags keep working: they name no command, so they
		// stay payloads and reach the signup source unchanged.
		{"/start SEMACOMEBACKTOVILLAGE", "", false},
		{"/start", "", false},
		// A command that exists but is not host-handled must not be
		// summoned this way — it would run outside the model's turn.
		{"/start help", "", false},
		// Only /start. Any two-word message whose second word happens to
		// name a command would otherwise fire it: "а что за plus" is a
		// question, and answering it by opening a payment sheet is the
		// bot taking a sentence as an instruction.
		{"/help plus", "", false},
		{"скажи plus", "", false},
		{"/plus plus", "", false},
	} {
		name, run := g.startPayloadCommand(tc.text)
		if run != tc.wantRun || name != tc.wantName {
			t.Errorf("%q → (%q, %v), want (%q, %v)", tc.text, name, run, tc.wantName, tc.wantRun)
		}
	}
}

func gatewayWithCommands(cmds []core.BotCommand) *Gateway {
	cfg := core.Config{}
	cfg.Gateway.Commands = cmds
	return &Gateway{deps: &core.Deps{Config: &cfg}}
}
