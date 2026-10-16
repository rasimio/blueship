package gateway

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

// textApprover is a host that only implements the original hook.
type textApprover struct {
	message string
	err     error
}

func (a textApprover) CompleteDeeplinkLogin(context.Context, string, int64) (bool, string, error) {
	return a.err == nil, a.message, a.err
}

// buttonApprover also implements DeeplinkLoginReplier; the gateway must
// prefer it.
type buttonApprover struct {
	textApprover
	reply    bs.DeeplinkReply
	err      error
	token    string
	tgUserID int64
}

func (a *buttonApprover) ReplyDeeplinkLogin(_ context.Context, token string, tgUserID int64) (bs.DeeplinkReply, error) {
	a.token, a.tgUserID = token, tgUserID
	return a.reply, a.err
}

func deeplinkGateway(t *testing.T, f *fakeBotAPI, approver bs.DeeplinkLoginApprover, cfg *bs.Config) (*Gateway, *botInstance) {
	t.Helper()
	g := &Gateway{
		deps:   &bs.Deps{Config: cfg, DeeplinkLogin: approver},
		logger: slog.New(slog.DiscardHandler),
		users:  map[string]*UserState{},
	}
	bi := &botInstance{
		id:         uuid.New(),
		kind:       "platform",
		tgUsername: "TestBot",
		client:     telegram.NewClientWithAPIURL("t", f.srv.URL, 5*time.Second),
	}
	return g, bi
}

func TestDeeplinkLoginReplyCarriesTheHostButton(t *testing.T) {
	f := newFakeBotAPI(t)
	approver := &buttonApprover{
		textApprover: textApprover{message: "the old line"},
		reply: bs.DeeplinkReply{
			Text:        "Approved.",
			ButtonLabel: "Back to the website",
			ButtonURL:   "https://example.test/finish?token=abc&code=xyz",
		},
	}
	g, bi := deeplinkGateway(t, f, approver, nil)

	if !g.maybeRunDeeplinkLogin(context.Background(), bi, 123, 456, "/start login_abc") {
		t.Fatal("login deep link was not handled")
	}
	if approver.token != "abc" || approver.tgUserID != 456 {
		t.Fatalf("hook input = (%q, %d)", approver.token, approver.tgUserID)
	}
	sent := f.last("sendMessage")
	if sent == nil {
		t.Fatalf("no sendMessage; calls = %v", f.methods())
	}
	if sent["text"] != "Approved." || sent["chat_id"] != float64(123) {
		t.Fatalf("sent = %v", sent)
	}
	if _, ok := sent["parse_mode"]; ok {
		t.Fatalf("reply sent with parse_mode %v; host text must go out as written", sent["parse_mode"])
	}
	preview, _ := sent["link_preview_options"].(map[string]any)
	if preview["is_disabled"] != true {
		t.Fatalf("link preview not disabled: %v", sent["link_preview_options"])
	}
	markup, _ := sent["reply_markup"].(map[string]any)
	rows, _ := markup["inline_keyboard"].([]any)
	if len(rows) != 1 {
		t.Fatalf("keyboard = %v, want one row", markup)
	}
	row, _ := rows[0].([]any)
	button, _ := row[0].(map[string]any)
	if button["text"] != "Back to the website" || button["url"] != "https://example.test/finish?token=abc&code=xyz" {
		t.Fatalf("button = %v", button)
	}
}

func TestDeeplinkLoginPlainApproverStillSendsItsLine(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := deeplinkGateway(t, f, textApprover{message: "Logged in."}, nil)

	if !g.maybeRunDeeplinkLogin(context.Background(), bi, 123, 456, "/start login_abc") {
		t.Fatal("login deep link was not handled")
	}
	sent := f.last("sendMessage")
	if sent["text"] != "Logged in." {
		t.Fatalf("sent = %v", sent)
	}
	if _, ok := sent["reply_markup"]; ok {
		t.Fatalf("plain approver got a keyboard: %v", sent["reply_markup"])
	}
}

func TestDeeplinkFailuresUseTheHostWording(t *testing.T) {
	cfg := &bs.Config{}
	cfg.UI.DeeplinkLoginFailed = "Вход не удался — начни снова на сайте."
	cfg.UI.DeeplinkLinkFailed = "Подключить не удалось — открой Настройки на сайте."

	t.Run("login", func(t *testing.T) {
		f := newFakeBotAPI(t)
		approver := &buttonApprover{
			reply: bs.DeeplinkReply{Text: "ignored", ButtonLabel: "x", ButtonURL: "https://example.test"},
			err:   errors.New("db down"),
		}
		g, bi := deeplinkGateway(t, f, approver, cfg)
		g.maybeRunDeeplinkLogin(context.Background(), bi, 1, 1, "/start login_abc")
		sent := f.last("sendMessage")
		if sent["text"] != cfg.UI.DeeplinkLoginFailed {
			t.Fatalf("text = %v", sent["text"])
		}
		if _, ok := sent["reply_markup"]; ok {
			t.Fatal("a failed hook still sent its button")
		}
	})
	t.Run("link", func(t *testing.T) {
		f := newFakeBotAPI(t)
		g, bi := deeplinkGateway(t, f, nil, cfg)
		g.deps.DeeplinkLink = failingLinker{}
		g.maybeRunDeeplinkLink(context.Background(), bi, 1, 1, "/start link_abc")
		if sent := f.last("sendMessage"); sent["text"] != cfg.UI.DeeplinkLinkFailed {
			t.Fatalf("text = %v", sent["text"])
		}
	})
	t.Run("defaults without a config", func(t *testing.T) {
		f := newFakeBotAPI(t)
		g, bi := deeplinkGateway(t, f, textApprover{err: errors.New("db down")}, nil)
		g.maybeRunDeeplinkLogin(context.Background(), bi, 1, 1, "/start login_abc")
		if sent := f.last("sendMessage"); sent["text"] != "Login link expired — try again on the website." {
			t.Fatalf("text = %v", sent["text"])
		}
	})
}

type failingLinker struct{}

func (failingLinker) CompleteDeeplinkLink(context.Context, string, uuid.UUID, int64, int64) (string, error) {
	return "", errors.New("db down")
}
