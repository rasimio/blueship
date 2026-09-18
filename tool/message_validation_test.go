package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	bs "github.com/rasimio/blueship/internal/core"
)

func TestMessageSendCannotPublishUnvalidatedToolInput(t *testing.T) {
	for _, mode := range []string{"replace", "reject", "empty"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			deps := &bs.Deps{UserID: uuid.New(), Config: &bs.Config{ResponseValidator: func(_ context.Context, req bs.ResponseValidationRequest) (string, error) {
				if req.Text != "unverified promise" || req.UserText != "request" || len(req.PendingTools) != 0 || len(req.Tools) != 1 || req.Tools[0].Name != "mutation" {
					t.Fatalf("invalid send evidence: %+v", req)
				}
				if mode == "reject" {
					return "", errors.New("unsupported")
				}
				if mode == "empty" {
					return "", nil
				}
				return "safe replacement", nil
			}}, SendConversationMessage: func(_ context.Context, _ uuid.UUID, text string) error {
				calls++
				if text != "safe replacement" {
					t.Fatalf("unsafe send: %q", text)
				}
				return nil
			}}
			registry := bs.NewToolRegistry()
			RegisterBuiltinTools(registry, deps)
			ctx := bs.WithResponseValidationContext(context.Background(), bs.ResponseValidationRequest{UserText: "request", PendingTools: []string{"message_send"}, Tools: []bs.ToolExecutionResult{{Name: "mutation"}}})
			out, failed := registry.Execute(ctx, ToolMessageSend, json.RawMessage(`{"text":"unverified promise"}`))
			if mode == "replace" && (failed || calls != 1) {
				t.Fatalf("replacement failed=%v sends=%d output=%s", failed, calls, out)
			}
			if mode != "replace" && (!failed || calls != 0) {
				t.Fatalf("unvalidated text escaped: sends=%d failed=%v", calls, failed)
			}
		})
	}
}
