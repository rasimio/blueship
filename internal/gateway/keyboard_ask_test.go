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

func askGateway(t *testing.T, f *fakeBotAPI) (*Gateway, *botInstance) {
	t.Helper()
	cfg := &bs.Config{}
	cfg.Gateway.Keyboard = bs.BotKeyboard{
		Root: "main", BackLabel: "‹ Назад", CloseLabel: "Закрыть", Closed: "Закрыла.",
		Nodes: map[string]bs.BotKeyboardNode{
			"main": {Text: "Меню.", Rows: [][]bs.BotKeyboardKey{
				{{Label: "🎨 Нарисовать картинку", Ask: "Что нарисовать?", Say: "Нарисуй: %s"}},
				{{Label: "✨ Умения", Node: "skills"}},
			}},
			"skills": {Text: "Что попробовать", Parent: "main", Rows: [][]bs.BotKeyboardKey{
				{{Label: "🕐 Часовой пояс",
					Say: "Смени мой часовой пояс — спроси, какой мне нужен, и запомни."}},
			}},
		},
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

// A demonstration key that acts on the tap has to make the request up on
// the person's behalf, and that invented request lands in memory as
// though they had said it. The question is the whole tap: nothing goes
// to the model until they answer.
func TestKeyboardAskSendsTheQuestionAndNothingElse(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := askGateway(t, f)

	text, handled := g.handleKeyboardTap(context.Background(), bi, 42, "🎨 Нарисовать картинку")
	if !handled {
		t.Fatal("the tapping key was not handled")
	}
	if text != "" {
		t.Fatalf("the tap handed %q to the pipeline, want nothing", text)
	}
	question := f.last("sendMessage")
	if question["text"] != "Что нарисовать?" {
		t.Fatalf("sent %v, want the key's question", question["text"])
	}
	if _, still := g.takeKeyboardAsk(bi.id, tgCanonical(42)); !still {
		t.Fatal("no question is open, so the answer would never compose")
	}
}

// The answer is the argument the key was missing: only the composed
// request reaches the pipeline, and it is consumed once.
func TestKeyboardAskComposesTheAnswerOnce(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := askGateway(t, f)
	g.handleKeyboardTap(context.Background(), bi, 42, "🎨 Нарисовать картинку")

	if got := g.answerKeyboardAsk(bi, 42, "  рыжего кота в скафандре  "); got != "Нарисуй: рыжего кота в скафандре" {
		t.Fatalf("composed %q, want the template with the answer in it", got)
	}
	if got := g.answerKeyboardAsk(bi, 42, "рыжего кота"); got != "рыжего кота" {
		t.Fatalf("the question answered twice: second message composed into %q", got)
	}
}

// A command is not an answer. "/stop" typed at an open question stops
// the turn; it does not become the subject of a drawing.
func TestKeyboardAskLeavesCommandsAndWordlessMessagesAlone(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := askGateway(t, f)

	g.handleKeyboardTap(context.Background(), bi, 42, "🎨 Нарисовать картинку")
	if got := g.answerKeyboardAsk(bi, 42, "/stop"); got != "/stop" {
		t.Fatalf("a command was composed into %q", got)
	}

	g.handleKeyboardTap(context.Background(), bi, 42, "🎨 Нарисовать картинку")
	if got := g.answerKeyboardAsk(bi, 42, "   "); got != "   " {
		t.Fatalf("a message with no words was composed into %q", got)
	}
}

// A question left open overnight must not rewrite the morning's message
// into yesterday's request.
func TestKeyboardAskGoesStale(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := askGateway(t, f)
	g.setKeyboardAsk(bi.id, tgCanonical(42), "Нарисуй: %s")
	g.kbAsk[telegramUserCacheKey(bi.id, tgCanonical(42))] = kbAskEntry{
		template: "Нарисуй: %s",
		at:       time.Now().Add(-2 * keyboardAskTTL),
	}

	if got := g.answerKeyboardAsk(bi, 42, "рыжего кота"); got != "рыжего кота" {
		t.Fatalf("a stale question composed %q", got)
	}
}

// Changing screens is changing the subject: the question closes with the
// screen it was asked on.
func TestKeyboardNavigationDropsAnOpenQuestion(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := askGateway(t, f)
	g.handleKeyboardTap(context.Background(), bi, 42, "🎨 Нарисовать картинку")

	if _, handled := g.handleKeyboardTap(context.Background(), bi, 42, "✨ Умения"); !handled {
		t.Fatal("navigation was not handled")
	}
	if got := g.answerKeyboardAsk(bi, 42, "рыжего кота"); got != "рыжего кота" {
		t.Fatalf("a question survived the screen change and composed %q", got)
	}
}

// Choosing another key is choosing another request: two open questions
// would race for the same next message.
func TestKeyboardKeyChoiceReplacesAnOpenQuestion(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := askGateway(t, f)
	g.handleKeyboardTap(context.Background(), bi, 42, "🎨 Нарисовать картинку")

	say, handled := g.handleKeyboardTap(context.Background(), bi, 42, "🕐 Часовой пояс")
	if handled {
		t.Fatal("a sentence key was swallowed instead of reaching the pipeline")
	}
	if say != "Смени мой часовой пояс — спроси, какой мне нужен, и запомни." {
		t.Fatalf("the key handed %q to the pipeline", say)
	}
	if got := g.answerKeyboardAsk(bi, 42, "рыжего кота"); got != "рыжего кота" {
		t.Fatalf("the replaced question still composed %q", got)
	}
}
