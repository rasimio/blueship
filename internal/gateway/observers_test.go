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

// watchingOnboarding is a host that watches everything the gateway offers
// to report.
type watchingOnboarding struct {
	noopOnboarding
	mu        sync.Mutex
	starts    []bs.StartSeen
	delivered []bs.DeliverySeen
	approved  []bs.PaymentApproved
	panics    bool
}

func (w *watchingOnboarding) ObserveStart(_ context.Context, s bs.StartSeen) {
	if w.panics {
		panic("host bug")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.starts = append(w.starts, s)
}
func (w *watchingOnboarding) ObserveDelivery(_ context.Context, d bs.DeliverySeen) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.delivered = append(w.delivered, d)
}
func (w *watchingOnboarding) ObservePaymentApproved(_ context.Context, p bs.PaymentApproved) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.approved = append(w.approved, p)
}

func watchedGateway(t *testing.T, host *watchingOnboarding) (*Gateway, *botInstance) {
	t.Helper()
	observeInline = true
	t.Cleanup(func() { observeInline = false })
	f := newFakeBotAPI(t)
	cfg := &bs.Config{}
	cfg.Gateway.Commands = payMenu
	g := &Gateway{
		deps: &bs.Deps{Config: cfg, BotOnboarding: host,
			CommandHandler: func(_ context.Context, in bs.BotCommandRequest) (bs.BotCommandResult, error) {
				return bs.BotCommandResult{Text: "Выбери, как заплатить:", Buttons: []bs.BotCommandButton{
					{Label: "500 ₽", URL: "https://yoomoney.ru/checkout/x"},
				}}, nil
			}},
		logger: slog.New(slog.DiscardHandler),
		users:  map[string]*UserState{},
	}
	bi := &botInstance{id: uuid.New(), tgUsername: "TestBot",
		client: telegram.NewClientWithAPIURL("t", f.srv.URL, 5*time.Second)}
	return g, bi
}

func startMsg(text, chatType string) *telegram.Message {
	m := &telegram.Message{MessageID: 77, Date: 1790000000, From: &telegram.User{ID: 555}, Text: text}
	m.Chat = telegram.Chat{ID: 555, Type: chatType}
	return m
}

func TestObserveStartReportsAcquisitionStartsOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		text    string
		chat    string
		want    bool
		payload string
	}{
		{"labelled", "/start tgads_lifeC_us0916", "private", true, "tgads_lifeC_us0916"},
		{"site token", "/start vs_AbCdEfGhIjKlMnOpQrStUv", "private", true, "vs_AbCdEfGhIjKlMnOpQrStUv"},
		{"bare", "/start", "private", true, ""},
		{"not a link's alphabet", "/start hello world", "private", true, ""},
		{"addressed to this bot", "/start@TestBot site_ru", "private", true, "site_ru"},
		{"errand", "/start plus", "private", false, ""},
		{"login", "/start login_abc", "private", false, ""},
		{"account link", "/start link_abc", "private", false, ""},
		{"a group", "/start tgads_x", "group", false, ""},
		{"another bot", "/start@OtherBot tgads_x", "private", false, ""},
		{"not a start", "hello", "private", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &watchingOnboarding{}
			g, bi := watchedGateway(t, host)
			g.observeStart(context.Background(), bi, startMsg(tc.text, tc.chat), tc.text)
			if got := len(host.starts) == 1; got != tc.want {
				t.Fatalf("reported=%v want %v", got, tc.want)
			}
			if !tc.want {
				return
			}
			s := host.starts[0]
			if s.Payload != tc.payload || s.MessageID != 77 || s.TGUserID != 555 || s.TGChatID != 555 || s.BotID != bi.id {
				t.Errorf("seen=%+v", s)
			}
			if !s.At.Equal(time.Unix(1790000000, 0)) {
				t.Errorf("at=%v, want Telegram's own date", s.At)
			}
		})
	}
}

// A host's bug must never cost the person their reply.
func TestObserverPanicIsContained(t *testing.T) {
	g, bi := watchedGateway(t, &watchingOnboarding{panics: true})
	g.observeStart(context.Background(), bi, startMsg("/start x", "private"), "/start x")
}

func TestHostCommandDeliveryIsReportedWithWhatItOffered(t *testing.T) {
	host := &watchingOnboarding{}
	g, bi := watchedGateway(t, host)
	us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
	if !g.maybeRunHostCommand(context.Background(), bi, 42, 555, us, "/plus") {
		t.Fatal("not consumed")
	}
	if len(host.delivered) != 1 {
		t.Fatalf("delivered=%+v", host.delivered)
	}
	d := host.delivered[0]
	if d.Kind != bs.DeliveryHostCommand || d.Name != "plus" || d.MessageID != 501 || d.URLButtons != 1 || d.TGChatID != 42 {
		t.Errorf("seen=%+v", d)
	}
}

func TestDenialDeliveryIsReportedWithItsReason(t *testing.T) {
	host := &watchingOnboarding{}
	g, bi := watchedGateway(t, host)
	g.sendDenial(context.Background(), bi, 42, "daily_quota_exhausted", "На сегодня всё.",
		[]bs.DecisionAction{{Label: "Подключить Plus", Command: "plus"}})
	g.sendDenial(context.Background(), bi, 42, "blocked", "Нельзя.", nil)
	if len(host.delivered) != 2 {
		t.Fatalf("delivered=%+v", host.delivered)
	}
	if d := host.delivered[0]; d.Kind != bs.DeliveryDenial || d.Name != "daily_quota_exhausted" || d.CallbackButtons != 1 || d.MessageID != 501 {
		t.Errorf("with a button: %+v", d)
	}
	if d := host.delivered[1]; d.Name != "blocked" || d.CallbackButtons != 0 || d.MessageID != 501 {
		t.Errorf("text only: %+v", d)
	}
}

// A gateway whose host watches nothing behaves exactly as before.
func TestNoObserverNoCalls(t *testing.T) {
	g, bi := watchedGateway(t, &watchingOnboarding{})
	g.deps.BotOnboarding = noopOnboarding{}
	g.observeStart(context.Background(), bi, startMsg("/start x", "private"), "/start x")
	g.sendDenial(context.Background(), bi, 42, "r", "t", nil)
}
