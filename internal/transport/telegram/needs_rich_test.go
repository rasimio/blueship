package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Text inside a Telegram Rich Message cannot be selected and copied.
// Sending every reply as one produced a bot whose answers you could read
// and not use — an address, a command, a link, all locked in the
// message. Ordinary messages render bold, italics and links as copyable
// entities, so rich is reserved for what only it can do.
func TestNeedsRichOnlyForWhatOrdinaryMessagesCannotRender(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		// Conversation. All of this reads fine as an ordinary message.
		{"plain prose", "Привет. Лимит снимется сам после оплаты.", false},
		{"bold and italics", "Это **важно**, а это _нет_.", false},
		{"a link", "Оплата тут: https://vaelum.ai/paid", false},
		{"a bullet list", "- первое\n- второе\n- третье", false},
		{"a heading", "## Итог\n\nВсё готово.", false},
		{"a lone pipe", "Выбирай: карта | звёзды", false},
		{"inline code", "Напиши `/plus` и всё.", false},

		// Only these three need it.
		{"a table", "| банк | сумма |\n|---|---|\n| Сбер | 500 |", true},
		{"a table with alignment", "| a | b |\n|:---|---:|\n| 1 | 2 |", true},
		{"a fenced code block", "```go\nfmt.Println(1)\n```", true},
		{"a display formula", "$$x = y$$", true},
		{"a bracket formula", "\\[x = y\\]", true},
	} {
		if got := NeedsRich(tc.text); got != tc.want {
			t.Errorf("%s: NeedsRich = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A single column is not a table. Without the two-cell floor, a stray
// "|---" under a line containing a pipe turns ordinary prose into a Rich
// Message, and the reply stops being copyable for the sake of a table
// that is not there.
func TestOneColumnIsNotATable(t *testing.T) {
	if NeedsRich("Цена | 500\n|---") {
		t.Error("a one-column divider was taken for a table")
	}
	if !NeedsRich("Цена | Срок\n|---|---|\n| 500 | 30 |") {
		t.Error("a real two-column table was not recognised")
	}
}

// The routing is the point of all this: prose must go out as an ordinary
// message. Testing the predicate alone would leave the branch that uses
// it free to disappear.
func TestSendRichLongUsesTheOrdinaryPathForProse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		text       string
		wantMethod string
	}{
		{"prose", "Привет. Всё готово, лимит снят.", "sendMessage"},
		{"prose with bold", "Это **важно**.", "sendMessage"},
		{"a table", "| a | b |\n|---|---|\n| 1 | 2 |", "sendRichMessage"},
		{"code", "```go\nx := 1\n```", "sendRichMessage"},
	} {
		var called []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			called = append(called, parts[len(parts)-1])
			io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		}))

		err := NewClientWithAPIURL("token", srv.URL, time.Second).
			SendRichLong(context.Background(), 42, tc.text)
		srv.Close()
		if err != nil {
			t.Fatalf("%s: send: %v", tc.name, err)
		}
		if len(called) != 1 || called[0] != tc.wantMethod {
			t.Errorf("%s: called %v, want [%s]", tc.name, called, tc.wantMethod)
		}
	}
}
