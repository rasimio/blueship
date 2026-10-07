package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/transport/telegram"
)

func TestTaskStatusDownloadCallbackSendsCompleteDocument(t *testing.T) {
	for _, kind := range []string{"platform", "user"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeBotAPI(t)
			report := strings.Repeat("Проверенный источник: https://example.test/item\n", 300)
			var got bs.BotCommandRequest
			g, bi := menuGateway(t, f, func(_ context.Context, req bs.BotCommandRequest) (bs.BotCommandResult, error) {
				got = req
				return bs.BotCommandResult{Document: &bs.BotCommandDocument{Name: "result.md", MIME: "text/markdown", Data: []byte(report)}}, nil
			})
			bi.kind = kind
			g.deps.Config.Gateway.Commands = append(g.deps.Config.Gateway.Commands, bs.BotCommand{Name: "status", Host: true}, bs.BotCommand{Name: "task_result", Host: true})
			type upload struct{ path, chat, name, data string }
			uploads := make(chan upload, 2)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("parse upload: %v", err)
					http.Error(w, "invalid upload", 400)
					return
				}
				defer r.MultipartForm.RemoveAll()
				file, header, err := r.FormFile("document")
				if err != nil {
					t.Errorf("missing document: %v", err)
					http.Error(w, "missing document", 400)
					return
				}
				defer file.Close()
				data, err := io.ReadAll(file)
				if err != nil {
					t.Errorf("read document: %v", err)
				}
				uploads <- upload{r.URL.Path, r.FormValue("chat_id"), header.Filename, strings.TrimPrefix(string(data), "\uFEFF")}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"ok":true,"result":{"message_id":502}}`)
			}))
			defer srv.Close()
			bi.client = telegram.NewClientWithAPIURL("test-only", srv.URL, time.Second)
			us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
			g.users[telegramUserCacheKey(bi.id, tgCanonical(777))] = us
			cq := menuTap(hostCallbackPrefix + "task_result:abcd1234")
			cq.Message.Chat.ID = 777
			if !g.maybeHandleHostCallback(context.Background(), bi, cq) {
				t.Fatal("download callback was not handled")
			}
			if got.Name != "task_result" || got.Args != "abcd1234" || got.UserID != us.UserID || got.SoulID != us.SoulID || got.BotID != bi.id || got.BotKind != kind {
				t.Fatalf("wrong download scope: %+v", got)
			}
			select {
			case sent := <-uploads:
				if !strings.HasSuffix(sent.path, "/sendDocument") || sent.chat != "777" || sent.name != "result.md" || sent.data != report {
					t.Fatalf("incorrect download: path=%s chat=%s name=%s bytes=%d", sent.path, sent.chat, sent.name, len(sent.data))
				}
			default:
				t.Fatal("no document reached transport")
			}
		})
	}
}

// Status must remain available when the host would deny another model turn.
// Exercise inbound routing, not just the host-command dispatcher.
func TestTaskStatusIngressBypassesExecutionPolicyOnBothBotKinds(t *testing.T) {
	for _, kind := range []string{"platform", "user"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeBotAPI(t)
			var got bs.BotCommandRequest
			hostCalls, policyCalls := 0, 0
			g, bi := menuGateway(t, f, func(_ context.Context, req bs.BotCommandRequest) (bs.BotCommandResult, error) {
				hostCalls++
				got = req
				return bs.BotCommandResult{Text: "Running", Buttons: []bs.BotCommandButton{{Label: "Refresh", Command: "status"}}}, nil
			})
			bi.kind = kind
			g.deps.Config.Gateway.Commands = append(g.deps.Config.Gateway.Commands, bs.BotCommand{Name: "status", Host: true})
			g.deps.AuthorizeExecution = func(context.Context, bs.ExecutionRequest) (bs.ExecutionDecision, error) {
				policyCalls++
				return bs.ExecutionDecision{Allowed: false, Reason: "quota_exhausted"}, nil
			}
			us := &UserState{UserID: uuid.New(), SoulID: uuid.New()}
			g.users[telegramUserCacheKey(bi.id, tgCanonical(777))] = us
			msg := &telegram.Message{From: &telegram.User{ID: 777}}
			msg.Chat.ID, msg.Chat.Type = 777, "private"
			_, _, admitted := g.prepareTelegramInbound(context.Background(), bi, msg, "/status@TestBot abcd1234")
			if admitted || hostCalls != 1 || policyCalls != 0 {
				t.Fatalf("admitted=%v host calls=%d policy calls=%d", admitted, hostCalls, policyCalls)
			}
			if got.Name != "status" || got.Args != "abcd1234" || got.UserID != us.UserID || got.SoulID != us.SoulID || got.BotID != bi.id || got.BotKind != kind {
				t.Fatalf("wrong command scope: %+v", got)
			}
			body := f.last("sendMessage")
			if body["text"] != "Running" || buttons(body)["Refresh"] != "host:status:" {
				t.Fatalf("status card did not reach transport: %+v", body)
			}
		})
	}
}
