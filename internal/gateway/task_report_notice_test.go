package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	bs "github.com/rasimio/blueship/internal/core"
)

func TestReportNoticeCarriesOpenReportButton(t *testing.T) {
	f := newFakeBotAPI(t)
	g, bi := menuGateway(t, f, nil)
	g.deps.Config.Gateway.Commands = append(g.deps.Config.Gateway.Commands, bs.BotCommand{Name: "task_result", Host: true})
	g.deps.Config.UI.TaskReportButton = "Открыть отчёт"
	g.bots = map[uuid.UUID]*botInstance{bi.id: bi}
	user, soul, id := uuid.New(), uuid.New(), uuid.New()
	g.deps.ResolveTelegramChat = func(context.Context, uuid.UUID, int64) (uuid.UUID, uuid.UUID, error) { return user, soul, nil }
	ctx := bs.ContextWithTaskOrigin(bs.WithSoulID(context.Background(), soul), bs.TaskOrigin{Transport: "telegram", BotID: bi.id, ChatID: "777"})
	notice := bs.TaskReportNotification("Готово: «%s»", "Рынок", "# Рынок\n\nЛидеры — **Vapi** и Retell.", id)
	receipt, err := g.SendToUserOnce(ctx, user, notice)
	if err != nil || receipt.MessageID != "501" {
		t.Fatal(receipt, err)
	}
	body := f.last("sendMessage")
	text := fmt.Sprint(body["text"])
	if !strings.Contains(text, "Готово: «Рынок»") || !strings.Contains(text, "<b>Vapi</b>") || strings.Contains(text, "task_report") {
		t.Fatalf("notice text: %q", text)
	}
	keyboard := fmt.Sprint(body["reply_markup"])
	if !strings.Contains(keyboard, "Открыть отчёт") || !strings.Contains(keyboard, hostCallbackPrefix+"task_result:"+id.String()) {
		t.Fatalf("no open-report button: %s", keyboard)
	}
}
