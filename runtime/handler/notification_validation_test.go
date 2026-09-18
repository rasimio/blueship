package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

func TestBackgroundValidatesFinalNotificationWithActualReceipts(t *testing.T) {
	for _, reject := range []bool{false, true} {
		provider := &capturingProvider{responses: []*core.CompletionResponse{
			{StopReason: "tool_use", Content: []core.ContentBlock{{Type: "tool_use", ID: "write", Name: "memory_update", Input: json.RawMessage(`{"id":"note"}`)}}},
			{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "[DONE] unverified promise"}}},
		}}
		deps, _ := backgroundTestDeps(provider, nil)
		deps.Registry.Register("memory_update", "test", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) {
			return map[string]string{"id": "note", "receipt": strings.Repeat("full", 300)}, nil
		})
		checks := 0
		deps.Config.ResponseValidator = func(_ context.Context, req core.ResponseValidationRequest) (string, error) {
			checks++
			if req.Text != "unverified promise" || len(req.Tools) != 1 || len(req.Tools[0].Output) < 1000 || req.SessionID == "" || req.Timezone != "UTC" || len(req.PendingTools) != 0 {
				t.Fatalf("notification validation got wrong boundary/evidence: %+v", req)
			}
			if reject {
				return "", errors.New("unsupported")
			}
			return "safe notification", nil
		}
		task := core.AgentTask{ID: uuid.New(), SoulID: uuid.New(), UserID: uuid.New(), Title: "check", Strategy: core.StrategyDirect, MaxIterations: 3, Config: json.RawMessage(`{"prompt":"finish","skip_reflex":true}`)}
		out, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps)
		if checks != 1 {
			t.Fatalf("checks=%d err=%v", checks, err)
		}
		if reject && (err == nil || out.Notify != "") {
			t.Fatalf("rejected notification escaped: %+v %v", out, err)
		}
		if !reject && (err != nil || out.Notify != "safe notification") {
			t.Fatalf("safe notification lost: %+v %v", out, err)
		}
	}
}

func TestKeyedDeliveryCannotAcknowledgeRewrittenNotification(t *testing.T) {
	_, err := validateBackgroundNotification(context.Background(), func(context.Context, core.ResponseValidationRequest) (string, error) { return "different subject", nil }, core.ResponseValidationRequest{}, "reminder subject", true)
	if err == nil {
		t.Fatal("rewritten text still acknowledged original delivery items")
	}
}
