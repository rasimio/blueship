package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

func directGateway(t *testing.T, reply func(w http.ResponseWriter, form map[string]string)) (*Gateway, uuid.UUID, *[]map[string]string) {
	t.Helper()
	var forms []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		form := map[string]string{"method": r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]}
		for k, v := range r.MultipartForm.Value {
			form[k] = v[0]
		}
		forms = append(forms, form)
		reply(w, form)
	}))
	t.Cleanup(srv.Close)
	cfg := &bs.Config{}
	cfg.Gateway.Commands = []bs.BotCommand{{Name: "mailing_off", Host: true}, {Name: "help", Prompt: "?"}}
	g := &Gateway{deps: &bs.Deps{Config: cfg}, logger: slog.New(slog.DiscardHandler)}
	g.initBotRegistry()
	id := uuid.New()
	g.bots[id] = &botInstance{id: id, client: telegram.NewClientWithAPIURL("1:T", srv.URL, 2*time.Second)}
	return g, id, &forms
}

func TestSendDirectDeliversThroughTheBotWithItsKeyboard(t *testing.T) {
	g, botID, forms := directGateway(t, func(w http.ResponseWriter, _ map[string]string) {
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":9}}`)
	})
	receipt, err := g.SendDirect(context.Background(), bs.DirectMessage{
		BotID: botID, ChatID: 55, HTML: "<i>Новости</i>", Silent: true, Bulk: true,
		Buttons: [][]bs.DirectButton{
			{{Text: "Открыть", URL: "https://vaelum.ai"}},
			{{Text: "Отключить рассылки", Command: "mailing_off", Args: "b1"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.MessageID != 9 || receipt.DeliveredAt.IsZero() {
		t.Fatalf("receipt = %+v", receipt)
	}
	form := (*forms)[0]
	if form["method"] != "sendMessage" || form["text"] != "<i>Новости</i>" || form["disable_notification"] != "true" {
		t.Fatalf("form = %v", form)
	}
	if !strings.Contains(form["reply_markup"], `"callback_data":"host:mailing_off:b1"`) {
		t.Fatalf("unsubscribe button not a host callback: %s", form["reply_markup"])
	}
}

func TestSendDirectBotNotRunningIsNotSent(t *testing.T) {
	g, _, forms := directGateway(t, func(http.ResponseWriter, map[string]string) {})
	_, err := g.SendDirect(context.Background(), bs.DirectMessage{BotID: uuid.New(), ChatID: 1, HTML: "x"})
	var de *bs.DirectSendError
	if !errors.As(err, &de) || de.Outcome != bs.DirectNotSent {
		t.Fatalf("err = %v", err)
	}
	if len(*forms) != 0 {
		t.Fatal("a request went out for a bot that is not running")
	}
}

func TestSendDirectRefusesAButtonItCannotBuild(t *testing.T) {
	g, botID, forms := directGateway(t, func(http.ResponseWriter, map[string]string) {})
	for name, b := range map[string]bs.DirectButton{
		"unknown command": {Text: "x", Command: "nope"},
		"prompt command":  {Text: "x", Command: "help"},
		"too long":        {Text: "x", Command: "mailing_off", Args: strings.Repeat("a", 60)},
		"both":            {Text: "x", Command: "mailing_off", URL: "https://a"},
		"neither":         {Text: "x"},
		"no label":        {URL: "https://a"},
	} {
		_, err := g.SendDirect(context.Background(), bs.DirectMessage{BotID: botID, ChatID: 1, HTML: "x", Buttons: [][]bs.DirectButton{{b}}})
		var de *bs.DirectSendError
		if !errors.As(err, &de) || de.Outcome != bs.DirectRejected {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(*forms) != 0 {
		t.Fatal("a message went out without the keyboard it was composed with")
	}
}

func TestClassifyDirectFailure(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		want  bs.DirectOutcome
		after time.Duration
	}{
		{"flood", &telegram.APIError{StatusCode: 429, ErrorCode: 429, Parameters: telegram.ResponseParameters{RetryAfter: 4}}, bs.DirectRateLimited, 4 * time.Second},
		{"blocked", &telegram.APIError{StatusCode: 403, ErrorCode: 403, Description: "Forbidden: bot was blocked by the user"}, bs.DirectRejected, 0},
		{"chat gone", &telegram.APIError{StatusCode: 400, ErrorCode: 400, Description: "Bad Request: chat not found"}, bs.DirectRejected, 0},
		{"proxy timeout", &telegram.APIError{StatusCode: 408}, bs.DirectUnknown, 0},
		{"server error", &telegram.APIError{StatusCode: 502}, bs.DirectUnknown, 0},
		{"never left", &telegram.TransportError{Method: "sendMessage", Err: errors.New("connection refused")}, bs.DirectNotSent, 0},
		{"lost answer", &telegram.TransportError{Method: "sendMessage", Written: true, Err: errors.New("timeout")}, bs.DirectUnknown, 0},
	}
	for _, c := range cases {
		got := classifyDirectFailure(c.err)
		if got.Outcome != c.want || got.RetryAfter != c.after {
			t.Errorf("%s: got %s after %v, want %s after %v", c.name, got.Outcome, got.RetryAfter, c.want, c.after)
		}
	}
}

// The unsubscribe button rides under a mailing; its answer must arrive on
// its own and leave the mailing in the chat.
func TestHostAnswerAsNewMessageLeavesTheTappedOne(t *testing.T) {
	g, bi, f, _ := screenGateway(t, noopOnboarding{}, func(context.Context, bs.BotCommandRequest) (bs.BotCommandResult, error) {
		return bs.BotCommandResult{Text: "Рассылки отключены.", NewMessage: true}, nil
	})
	g.maybeHandleHostCallback(context.Background(), bi, screenTap("memory:x"))
	if f.last("editMessageText") != nil {
		t.Fatal("the tapped message was replaced")
	}
	if sent := f.last("sendMessage"); sent == nil || sent["text"] != "Рассылки отключены." {
		t.Fatalf("answer not sent as a new message: %v", f.methods())
	}
}
