package gateway

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

type recordingDeeplinkLinker struct {
	called   bool
	token    string
	botID    uuid.UUID
	tgUserID int64
	tgChatID int64
}

func (r *recordingDeeplinkLinker) CompleteDeeplinkLink(_ context.Context, token string, botID uuid.UUID, tgUserID, tgChatID int64) (string, error) {
	r.called = true
	r.token = token
	r.botID = botID
	r.tgUserID = tgUserID
	r.tgChatID = tgChatID
	return "linked", nil
}

func TestDeeplinkLinkDelegatesUserBotAuthorizationToHost(t *testing.T) {
	linker := &recordingDeeplinkLinker{}
	botID := uuid.New()
	g := &Gateway{
		deps:   &bs.Deps{DeeplinkLink: linker},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		users:  make(map[string]*UserState),
	}
	bot := &botInstance{
		id:          botID,
		kind:        "user",
		ownerUserID: uuid.New(),
	}

	if !g.maybeRunDeeplinkLink(context.Background(), bot, 123, 456, "/start link_abc") {
		t.Fatal("user-owned bot deeplink was not handled")
	}
	if !linker.called {
		t.Fatal("host DeeplinkLinker was not called")
	}
	if linker.token != "abc" || linker.botID != botID || linker.tgUserID != 456 || linker.tgChatID != 123 {
		t.Fatalf("unexpected hook input: %+v", linker)
	}
}
