package gateway

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// A host command that is a small screen rather than a single answer: rows
// of buttons, text it did not write, pages that replace one another, and a
// question whose answer has to find its way back.

// parkedOnboarding keeps one person's state row the way the host's store
// does: data comes back as a copy, as it would through JSON.
type parkedOnboarding struct {
	noopOnboarding
	mu      sync.Mutex
	step    string
	data    map[string]any
	cleared int
}

func (p *parkedOnboarding) GetState(context.Context, int64, uuid.UUID) (string, map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	data := map[string]any{}
	for k, v := range p.data {
		data[k] = v
	}
	return p.step, data, nil
}

func (p *parkedOnboarding) AdvanceStep(_ context.Context, _ int64, _ uuid.UUID, step string, data map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.step, p.data = step, data
	return nil
}

func (p *parkedOnboarding) ClearState(context.Context, int64, uuid.UUID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.step, p.data = "", nil
	p.cleared++
	return nil
}

func (p *parkedOnboarding) state() (string, map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.step, p.data
}

// screenChat is the person's private chat with the bot: chat id = user id.
const screenChat = 777

func screenGateway(t *testing.T, onb bs.BotOnboarding, handler bs.BotCommandHandler) (*Gateway, *botInstance, *fakeBotAPI, *UserState) {
	t.Helper()
	f := newFakeBotAPI(t)
	cfg := &bs.Config{}
	cfg.Gateway.Commands = []bs.BotCommand{
		{Name: "memory", Description: "Память", Host: true},
		{Name: "memory_edit", Host: true},
		{Name: "plus", Description: "Подписка", Host: true},
	}
	cfg.Gateway.Keyboard = bs.BotKeyboard{
		Root: "main", CloseLabel: "Закрыть", Closed: "Убрала.", BackLabel: "Назад",
		Nodes: map[string]bs.BotKeyboardNode{
			"main": {Text: "Меню.", Rows: [][]bs.BotKeyboardKey{
				{{Label: "Настройки", Node: "settings"}},
			}},
			"settings": {Text: "Настройки.", Parent: "main", Rows: [][]bs.BotKeyboardKey{
				{{Label: "Подписка", Command: "plus"}},
			}},
		},
	}
	g := &Gateway{
		deps:   &bs.Deps{Config: cfg, CommandHandler: handler, BotOnboarding: onb},
		logger: slog.New(slog.DiscardHandler),
		users:  map[string]*UserState{},
	}
	bi := &botInstance{id: uuid.New(), tgUsername: "TestBot",
		client: telegram.NewClientWithAPIURL("t", f.srv.URL, 5*time.Second)}
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	g.users[telegramUserCacheKey(bi.id, tgCanonical(screenChat))] = us
	return g, bi, f, us
}

func screenCaller() bs.TelegramCaller {
	return bs.TelegramCaller{UserID: screenChat, ChatID: screenChat}
}

func screenTap(data string) *telegram.CallbackQuery {
	cq := menuTap(hostCallbackPrefix + data)
	cq.From.ID = screenChat
	cq.Message.Chat.ID = screenChat
	return cq
}

func screenSend(g *Gateway, bi *botInstance, text string) {
	msg := &telegram.Message{MessageID: 1, From: &telegram.User{ID: screenChat}, Text: text}
	msg.Chat = telegram.Chat{ID: screenChat, Type: "private"}
	g.handleUpdate(context.Background(), bi, telegram.Update{Message: msg})
}

// rowShape renders a recorded keyboard as "a b|c": labels in a row joined
// by spaces, rows by bars.
func rowShape(body map[string]any) string {
	markup, _ := body["reply_markup"].(map[string]any)
	rows, _ := markup["inline_keyboard"].([]any)
	shape := make([]string, 0, len(rows))
	for _, row := range rows {
		labels := []string{}
		for _, b := range row.([]any) {
			labels = append(labels, b.(map[string]any)["text"].(string))
		}
		shape = append(shape, strings.Join(labels, " "))
	}
	return strings.Join(shape, "|")
}

// Numbers to pick from belong on one row, and so do back and forward; a
// column of them reads as a list of unrelated choices. Text the host marks
// Plain is what the person saved: it must arrive with its dollar signs,
// asterisks and underscores intact.
func TestHostAnswerKeepsRowsAndPlainText(t *testing.T) {
	text := "Стоит $5, а не $10 — **не жирным**, a_b_c и <тег>"
	g, bi, f, us := screenGateway(t, noopOnboarding{}, func(context.Context, bs.BotCommandRequest) (bs.BotCommandResult, error) {
		return bs.BotCommandResult{Text: text, Plain: true, Buttons: []bs.BotCommandButton{
			{Label: "1", Command: "memory", Args: "o:1"},
			{Label: "2", Command: "memory", Args: "o:2", SameRow: true},
			{Label: "Далее", Command: "memory", Args: "v:1"},
			{Label: "Сайт", URL: "https://vaelum.ai", SameRow: true},
			{Label: "Закрыть", Command: "memory", Args: "x"},
		}}, nil
	})

	g.runHostCommand(context.Background(), bi, screenCaller(), us, "memory", "", 0)
	sent := f.last("sendMessage")
	if sent == nil {
		t.Fatalf("nothing sent: %v", f.methods())
	}
	if sent["text"] != text {
		t.Errorf("text = %q, want it as given", sent["text"])
	}
	if mode, ok := sent["parse_mode"]; ok {
		t.Errorf("plain text sent with parse_mode %v", mode)
	}
	if got := rowShape(sent); got != "1 2|Далее Сайт|Закрыть" {
		t.Errorf("rows = %q", got)
	}
}

// A page replaces the one it was turned from. When that message can no
// longer be edited — deleted, or too old — the page still has to arrive.
func TestHostAnswerToATapArrivesEvenWhenTheMessageCannotBeEdited(t *testing.T) {
	g, bi, f, _ := screenGateway(t, noopOnboarding{}, func(context.Context, bs.BotCommandRequest) (bs.BotCommandResult, error) {
		return bs.BotCommandResult{Text: "Страница 2", Plain: true,
			Buttons: []bs.BotCommandButton{{Label: "Назад", Command: "memory", Args: "v:0"}}}, nil
	})

	g.maybeHandleHostCallback(context.Background(), bi, screenTap("memory:v:1"))
	if got := f.methods(); strings.Join(got, ",") != "answerCallbackQuery,editMessageText" &&
		strings.Join(got, ",") != "editMessageText" {
		t.Fatalf("an editable message was not edited in place: %v", got)
	}

	f.mu.Lock()
	f.calls = nil
	f.fail = map[string]string{"editMessageText": "Bad Request: message to edit not found"}
	f.mu.Unlock()
	g.maybeHandleHostCallback(context.Background(), bi, screenTap("memory:v:1"))
	sent := f.last("sendMessage")
	if sent == nil || sent["text"] != "Страница 2" || rowShape(sent) != "Назад" {
		t.Fatalf("the page was lost with the message: calls %v, sent %v", f.methods(), sent)
	}

	// "Not modified" is Telegram saying the page is already there.
	f.mu.Lock()
	f.calls = nil
	f.fail = map[string]string{"editMessageText": "Bad Request: message is not modified"}
	f.mu.Unlock()
	g.maybeHandleHostCallback(context.Background(), bi, screenTap("memory:v:1"))
	if f.last("sendMessage") != nil {
		t.Fatal("an unchanged page was sent a second time")
	}
}

// Text longer than one message goes out whole, in parts; the buttons go
// under the last part.
func TestHostAnswerTooLongForOneMessageIsSplit(t *testing.T) {
	text := strings.Repeat("а", 3000) + "\n\n" + strings.Repeat("б", 3000)
	g, bi, f, _ := screenGateway(t, noopOnboarding{}, func(context.Context, bs.BotCommandRequest) (bs.BotCommandResult, error) {
		return bs.BotCommandResult{Text: text, Plain: true,
			Buttons: []bs.BotCommandButton{{Label: "К списку", Command: "memory", Args: "v:0"}}}, nil
	})

	g.maybeHandleHostCallback(context.Background(), bi, screenTap("memory:o:1"))
	edit, sent := f.last("editMessageText"), f.last("sendMessage")
	if edit == nil || sent == nil {
		t.Fatalf("calls = %v, want the tapped message edited and the rest sent", f.methods())
	}
	if got := edit["text"].(string) + sent["text"].(string); got != text {
		t.Errorf("the parts do not add up to the text: %d of %d runes", len([]rune(got)), len([]rune(text)))
	}
	if rowShape(edit) != "" || rowShape(sent) != "К списку" {
		t.Errorf("buttons: first part %q, last part %q", rowShape(edit), rowShape(sent))
	}
}

// A question asked from a tapped button waits for the next message from
// that chat, and hands its answer back with what the question was about.
func TestHostQuestionFromAButtonGetsItsAnswerWithItsState(t *testing.T) {
	onb := &parkedOnboarding{}
	var got []bs.BotCommandRequest
	g, bi, _, us := screenGateway(t, onb, func(_ context.Context, in bs.BotCommandRequest) (bs.BotCommandResult, error) {
		got = append(got, in)
		if in.Name == "memory" {
			return bs.BotCommandResult{Text: "Пришлите новый текст", AwaitReply: "memory_edit", AwaitState: "entry-7",
				Buttons: []bs.BotCommandButton{{Label: "Отмена", Command: "memory", Args: "o:7"}}}, nil
		}
		return bs.BotCommandResult{Text: "Сохранено"}, nil
	})

	g.maybeHandleHostCallback(context.Background(), bi, screenTap("memory:e:7"))
	step, data := onb.state()
	if step != onbStepAwaitPrefix+"memory_edit" || data[onbDataAwaitState] != "entry-7" || data[onbDataAwaitChat] != "777" {
		t.Fatalf("not waiting for the answer: step %q, data %v", step, data)
	}

	// Something said in a group is not the answer to a private question,
	// and does not end it either.
	if g.maybeRunBotOnboarding(context.Background(), bi, tgCanonical(-100), -100, screenChat, "в группе", tgSender{}) {
		t.Error("a group message was taken as the answer")
	}
	if step, _ := onb.state(); step == "" {
		t.Fatal("a group message ended the wait")
	}

	if !g.maybeRunBotOnboarding(context.Background(), bi, tgCanonical(screenChat), screenChat, screenChat, "  новый текст ", tgSender{}) {
		t.Fatal("the answer was not consumed")
	}
	last := got[len(got)-1]
	if last.Name != "memory_edit" || last.Args != "новый текст" || last.AwaitState != "entry-7" ||
		last.UserID != us.UserID || last.SoulID != us.SoulID || !last.Caller.Private() {
		t.Fatalf("answer delivered as %+v", last)
	}
	if step, _ := onb.state(); step != "" {
		t.Fatalf("still waiting after the answer: %q", step)
	}
}

// Typed by hand, the awaited command carries no state: only the question
// itself vouches for what the answer is about.
func TestHostCommandTypedByHandCarriesNoAwaitState(t *testing.T) {
	var got bs.BotCommandRequest
	g, bi, _, us := screenGateway(t, noopOnboarding{}, func(_ context.Context, in bs.BotCommandRequest) (bs.BotCommandResult, error) {
		got = in
		return bs.BotCommandResult{}, nil
	})
	g.maybeRunHostCommand(context.Background(), bi, screenCaller(), us, "/memory_edit текст")
	if got.Name != "memory_edit" || got.AwaitState != "" {
		t.Fatalf("typed command = %+v", got)
	}
}

// Tapping another button instead of answering moves on: the question ends
// unless the new answer asks one of its own. A question open in another
// chat is not this chat's to end.
func TestHostButtonThatAsksNothingEndsTheQuestion(t *testing.T) {
	onb := &parkedOnboarding{}
	g, bi, _, _ := screenGateway(t, onb, func(_ context.Context, in bs.BotCommandRequest) (bs.BotCommandResult, error) {
		return bs.BotCommandResult{Text: "Список", Plain: true}, nil
	})

	onb.AdvanceStep(context.Background(), screenChat, bi.id, onbStepAwaitPrefix+"memory_edit",
		map[string]any{onbDataAwaitChat: "555"})
	g.maybeHandleHostCallback(context.Background(), bi, screenTap("memory:v:0"))
	if step, _ := onb.state(); step == "" {
		t.Fatal("a tap in one chat ended a question asked in another")
	}

	onb.AdvanceStep(context.Background(), screenChat, bi.id, onbStepAwaitPrefix+"memory_edit",
		map[string]any{onbDataAwaitChat: "777"})
	g.maybeHandleHostCallback(context.Background(), bi, screenTap("memory:v:0"))
	if step, _ := onb.state(); step != "" {
		t.Fatalf("still waiting after the person tapped something else: %q", step)
	}

	// Anything else the row holds is not a question and stays.
	onb.AdvanceStep(context.Background(), screenChat, bi.id, onbStepOfferPending, map[string]any{onbDataTurns: 2})
	g.maybeHandleHostCallback(context.Background(), bi, screenTap("memory:v:0"))
	if step, _ := onb.state(); step != onbStepOfferPending {
		t.Fatalf("a pending offer was thrown away: %q", step)
	}
}

// A typed command or a key on the persistent keyboard is the person doing
// something else. The command must run as itself, not be saved as the
// answer — "/plus" as a corrected memory — and the question must end.
func TestHostQuestionEndsOnACommandOrAKey(t *testing.T) {
	onb := &parkedOnboarding{}
	var got []bs.BotCommandRequest
	g, bi, f, _ := screenGateway(t, onb, func(_ context.Context, in bs.BotCommandRequest) (bs.BotCommandResult, error) {
		got = append(got, in)
		return bs.BotCommandResult{Text: "ok"}, nil
	})
	park := func() {
		onb.AdvanceStep(context.Background(), screenChat, bi.id, onbStepAwaitPrefix+"memory_edit",
			map[string]any{onbDataAwaitChat: "777", onbDataAwaitState: "entry-7"})
	}

	park()
	screenSend(g, bi, "/plus")
	if len(got) != 1 || got[0].Name != "plus" || got[0].Args != "" {
		t.Fatalf("the command became an answer: %+v", got)
	}
	if step, _ := onb.state(); step != "" {
		t.Fatalf("still waiting after a command: %q", step)
	}

	park()
	screenSend(g, bi, "Настройки")
	if step, _ := onb.state(); step != "" {
		t.Fatalf("still waiting after a keyboard key: %q", step)
	}
	if len(got) != 1 || f.last("sendMessage") == nil {
		t.Fatalf("the key did not open its screen: handler %+v, calls %v", got, f.methods())
	}

	// Words are still the answer.
	park()
	screenSend(g, bi, "новый текст")
	if len(got) != 2 || got[1].Name != "memory_edit" || got[1].Args != "новый текст" || got[1].AwaitState != "entry-7" {
		t.Fatalf("the answer did not arrive: %+v", got)
	}
}
