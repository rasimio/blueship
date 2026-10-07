package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	bs "github.com/rasimio/blueship/internal/core"
)

func longNoticeGateway(t *testing.T, f *fakeBotAPI) (*Gateway, context.Context, uuid.UUID) {
	t.Helper()
	g, bi := menuGateway(t, f, nil)
	g.bots = map[uuid.UUID]*botInstance{bi.id: bi}
	user, soul := uuid.New(), uuid.New()
	g.deps.ResolveTelegramChat = func(context.Context, uuid.UUID, int64) (uuid.UUID, uuid.UUID, error) { return user, soul, nil }
	ctx := bs.ContextWithTaskOrigin(bs.WithSoulID(context.Background(), soul), bs.TaskOrigin{Transport: "telegram", BotID: bi.id, ChatID: "777"})
	return g, ctx, user
}

// A daily tale of ~6000 characters: Telegram rejects an ordinary message
// above 4096, and every message_send failed (production, 2026-09-24).
func longTale() string {
	paragraph := strings.Repeat("Жил-был старый рыбак у холодного моря. ", 20)
	return "Отчего море солёное\nНорвежская народная сказка\n\n" + strings.Repeat(paragraph+"\n\n", 8)
}

func TestSendToUserOnceDeliversLongTextAsOneRichMessage(t *testing.T) {
	f := newFakeBotAPI(t)
	g, ctx, user := longNoticeGateway(t, f)
	text := longTale()
	if utf8.RuneCountInString(text) <= 4096 {
		t.Fatal("fixture is not long")
	}
	receipt, err := g.SendToUserOnce(ctx, user, text)
	if err != nil || receipt.MessageID != "501" {
		t.Fatal(receipt, err)
	}
	if got := f.methods(); len(got) != 1 || got[0] != "sendRichMessage" {
		t.Fatalf("calls: %v", got)
	}
	rich, _ := json.Marshal(f.last("sendRichMessage")["rich_message"])
	if !strings.Contains(string(rich), "Отчего море солёное") || !strings.Contains(string(rich), "рыбак") {
		t.Fatalf("rich body: %s", rich)
	}
}

func TestSendToUserOnceSplitsLongTextWhenRichIsRejected(t *testing.T) {
	f := &fakeBotAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		method := parts[len(parts)-1]
		f.mu.Lock()
		f.calls = append(f.calls, apiCall{method: method, body: body})
		n := len(f.calls)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if method == "sendRichMessage" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: method not supported"}`)
			return
		}
		if text := fmt.Sprint(body["text"]); utf8.RuneCountInString(text) > 4096 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, 600+n)
	}))
	t.Cleanup(f.srv.Close)
	g, ctx, user := longNoticeGateway(t, f)

	receipt, err := g.SendToUserOnce(ctx, user, longTale())
	if err != nil {
		t.Fatal(err)
	}
	got := f.methods()
	if len(got) < 3 || got[0] != "sendRichMessage" || got[1] != "sendMessage" || got[len(got)-1] != "sendMessage" {
		t.Fatalf("calls: %v", got)
	}
	if receipt.MessageID != "602" {
		t.Fatalf("receipt should name the first part: %+v", receipt)
	}
	var delivered strings.Builder
	for _, c := range f.calls[1:] {
		delivered.WriteString(fmt.Sprint(c.body["text"]))
	}
	if !strings.HasPrefix(delivered.String(), "Отчего море солёное") || strings.Count(delivered.String(), "рыбак") != 160 {
		t.Fatalf("the tale was not delivered whole: %d runes", utf8.RuneCountInString(delivered.String()))
	}
}
