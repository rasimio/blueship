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

// fsmOnboarding is a BotOnboarding that only remembers a step, which is
// all these tests are about.
type fsmOnboarding struct {
	noopOnboarding
	step    string
	cleared bool
}

func (f *fsmOnboarding) GetState(context.Context, int64, uuid.UUID) (string, map[string]any, error) {
	return f.step, map[string]any{}, nil
}

func (f *fsmOnboarding) ClearState(context.Context, int64, uuid.UUID) error {
	f.cleared = true
	f.step = ""
	return nil
}

// A key on the persistent keyboard is one tap away from every wizard
// step, and a step that reads free text will take anything. Tapping
// «Начать заново» while the wizard asked for a name produced an
// assistant called "/reset" — the keys have to work wherever they are
// visible, or they are a trap rather than a menu.
func TestCommandInterruptsTheWizardInsteadOfBecomingAnAnswer(t *testing.T) {
	f := newFakeBotAPI(t)
	fsm := &fsmOnboarding{step: onbStepAskName}
	cfg := &bs.Config{}
	cfg.Gateway.Commands = []bs.BotCommand{{Name: "reset", Description: "Начать заново"}}
	cfg.Gateway.OnboardingFlow = bs.OnboardingFlow{
		Voices: []bs.OnboardingVoice{{ID: "clear", Name: "Ясный"}},
	}
	g := &Gateway{
		deps:   &bs.Deps{Config: cfg, BotOnboarding: fsm},
		logger: slog.New(slog.DiscardHandler),
		users:  map[string]*UserState{},
	}
	bi := &botInstance{id: uuid.New(), tgUsername: "TestBot",
		client: telegram.NewClientWithAPIURL("t", f.srv.URL, 5*time.Second)}

	consumed := g.maybeRunBotOnboarding(context.Background(), bi,
		tgCanonical(42), 42, 777, "/reset", tgSender{})

	if consumed {
		t.Error("the wizard swallowed a command; it must fall through to whoever owns it")
	}
	if !fsm.cleared {
		t.Error("the wizard is still waiting for a name after the person left it")
	}
	// Nothing was said: the command's own dispatcher answers.
	if len(f.methods()) != 0 {
		t.Errorf("the wizard replied to a command: %v", f.methods())
	}
}

// Ordinary text is still an answer — the interrupt must not eat the
// wizard's actual input.
func TestPlainTextStillAnswersTheWizard(t *testing.T) {
	f := newFakeBotAPI(t)
	fsm := &fsmOnboarding{step: onbStepAskName}
	cfg := &bs.Config{}
	cfg.Gateway.Commands = []bs.BotCommand{{Name: "reset", Description: "Начать заново"}}
	cfg.Gateway.OnboardingFlow = bs.OnboardingFlow{
		Voices: []bs.OnboardingVoice{{ID: "clear", Name: "Ясный"}},
	}
	g := &Gateway{
		deps:   &bs.Deps{Config: cfg, BotOnboarding: fsm},
		logger: slog.New(slog.DiscardHandler),
		users:  map[string]*UserState{},
	}
	bi := &botInstance{id: uuid.New(), tgUsername: "TestBot",
		client: telegram.NewClientWithAPIURL("t", f.srv.URL, 5*time.Second)}

	if !g.maybeRunBotOnboarding(context.Background(), bi,
		tgCanonical(42), 42, 777, "Нори", tgSender{}) {
		t.Fatal("a name was not consumed by the step asking for one")
	}
	if fsm.cleared {
		t.Error("answering the wizard threw the wizard away")
	}
}

// Second rubbing point: the interrupt above only recognises commands
// addressed to this bot. "/reset@SomeOtherBot" is not one, and would
// otherwise be accepted as what to call the assistant.
func TestNameStepRefusesAnythingStartingWithASlash(t *testing.T) {
	f := newFakeBotAPI(t)
	fsm := &fsmOnboarding{step: onbStepAskName}
	cfg := &bs.Config{}
	cfg.Gateway.Onboarding = bs.OnboardingMessages{NameTooShort: "не похоже на имя"}
	cfg.Gateway.OnboardingFlow = bs.OnboardingFlow{
		Voices: []bs.OnboardingVoice{{ID: "clear", Name: "Ясный"}},
	}
	g := &Gateway{
		deps:   &bs.Deps{Config: cfg, BotOnboarding: fsm},
		logger: slog.New(slog.DiscardHandler),
		users:  map[string]*UserState{},
	}
	bi := &botInstance{id: uuid.New(), tgUsername: "TestBot",
		client: telegram.NewClientWithAPIURL("t", f.srv.URL, 5*time.Second)}

	if !g.onboardingHandleName(context.Background(), bi, 42, 777, "/reset@SomeOtherBot", map[string]any{}) {
		t.Fatal("the step did not consume the message")
	}
	if got := f.last("sendMessage"); got == nil || got["text"] != "не похоже на имя" {
		t.Errorf("named the assistant after a command instead of refusing: %v", got)
	}
	// And a real name still gets through.
	if !g.onboardingHandleName(context.Background(), bi, 42, 777, "Нори", map[string]any{}) {
		t.Fatal("a real name was rejected")
	}
	if got := f.last("sendMessage"); got != nil && got["text"] == "не похоже на имя" {
		t.Error("a real name was refused as a command")
	}
}
