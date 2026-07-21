package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	bs "github.com/rasimio/blueship/internal/core"
)

type recordingMessageSender struct {
	chatID string
	text   string
	calls  int
}

func (s *recordingMessageSender) SendMessage(_ context.Context, chatID, text string) (int, error) {
	s.chatID = chatID
	s.text = text
	s.calls++
	return 42, nil
}

func (*recordingMessageSender) SendLong(context.Context, string, string) error { return nil }

func (*recordingMessageSender) SendVoice(context.Context, string, []byte) error { return nil }

func TestMessageSendSymbolicRecipientUsesInvocationUser(t *testing.T) {
	t.Parallel()

	legacy := &recordingMessageSender{}
	wantUserID := uuid.New()
	var gotUserID uuid.UUID
	var gotText string
	deps := &bs.Deps{
		Config: &bs.Config{Sender: legacy},
		UserID: wantUserID,
		SendToUser: func(_ context.Context, userID uuid.UUID, text string) error {
			gotUserID = userID
			gotText = text
			return nil
		},
	}

	registry := bs.NewToolRegistry()
	RegisterBuiltinTools(registry, deps)
	result, isError := registry.Execute(context.Background(), ToolMessageSend, json.RawMessage(`{"to":"owner","text":"private"}`))
	if isError {
		t.Fatalf("message_send returned an error: %s", result)
	}
	if gotUserID != wantUserID {
		t.Fatalf("SendToUser user ID = %s, want %s", gotUserID, wantUserID)
	}
	if gotText != "private" {
		t.Fatalf("SendToUser text = %q, want %q", gotText, "private")
	}
	if legacy.calls != 0 {
		t.Fatalf("legacy sender calls = %d, want 0", legacy.calls)
	}
}

func TestMessageSendTenantRecipientFailsClosedWithoutScopedSender(t *testing.T) {
	t.Parallel()

	legacy := &recordingMessageSender{}
	deps := &bs.Deps{
		Config: &bs.Config{
			Sender: legacy,
			Owner:  bs.OwnerConfig{ChatID: "telegram:123"},
		},
		UserID: uuid.New(),
	}

	registry := bs.NewToolRegistry()
	RegisterBuiltinTools(registry, deps)
	result, isError := registry.Execute(context.Background(), ToolMessageSend, json.RawMessage(`{"to":"owner","text":"private"}`))
	if !isError {
		t.Fatalf("message_send result = %s, want an error", result)
	}
	if legacy.calls != 0 {
		t.Fatalf("legacy sender calls = %d, want 0", legacy.calls)
	}
}

func TestMessageSendSingleUserFallbackUsesConfiguredOwner(t *testing.T) {
	t.Parallel()

	legacy := &recordingMessageSender{}
	deps := &bs.Deps{
		Config: &bs.Config{
			Sender: legacy,
			Owner:  bs.OwnerConfig{ChatID: "telegram:123"},
		},
	}

	registry := bs.NewToolRegistry()
	RegisterBuiltinTools(registry, deps)
	result, isError := registry.Execute(context.Background(), ToolMessageSend, json.RawMessage(`{"to":"owner","text":"legacy"}`))
	if isError {
		t.Fatalf("message_send returned an error: %s", result)
	}
	if legacy.calls != 1 {
		t.Fatalf("legacy sender calls = %d, want 1", legacy.calls)
	}
	if legacy.chatID != "123" {
		t.Fatalf("legacy sender chat ID = %q, want %q", legacy.chatID, "123")
	}
}
