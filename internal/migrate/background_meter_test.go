package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rasimio/blueship/internal/core"
)

type auditCaptureProvider struct{ core.CompletionProvider }

func (auditCaptureProvider) Complete(context.Context, core.CompletionRequest) (*core.CompletionResponse, error) {
	return &core.CompletionResponse{StopReason: "max_tokens", Content: []core.ContentBlock{{Type: "text", Text: `{"claims":[`}}}, errors.New("response interrupted")
}

func TestLiveMeterPreservesExactAuditInputOnFailure(t *testing.T) {
	text := "[report]\nЦена 18.597,60 RSD; 159 мм < 175 мм\n[fetched_documents]\n" + strings.Repeat("Контекст источника: цена, наличие, размеры.\n", 4000) + "END_OF_SOURCE"
	req := core.CompletionRequest{System: "You are a citation auditor\nStrict factual policy", Messages: []core.Message{{Role: "user", Content: core.NormalizeContent(text)}}, Model: "test", MaxTokens: 8192, Effort: "medium"}
	meter := liveCompletionMeter{provider: auditCaptureProvider{}}
	if _, err := meter.Complete(context.Background(), req); err == nil {
		t.Fatal("fixture did not fail")
	}
	data, err := json.Marshal(map[string]any{"calls": meter.calls})
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Calls []struct {
			System   string                   `json:"audit_system"`
			Messages []core.Message           `json:"audit_messages"`
			Failed   bool                     `json:"failed"`
			Response *core.CompletionResponse `json:"response"`
		} `json:"calls"`
	}
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Calls) != 1 {
		t.Fatal(len(saved.Calls))
	}
	call := saved.Calls[0]
	if call.System != req.System || len(call.Messages) != 1 || core.ExtractText(core.NormalizeContent(call.Messages[0].Content)) != text || !call.Failed || call.Response == nil || call.Response.StopReason != "max_tokens" {
		t.Fatal("exact audit input or failure evidence lost during JSON round-trip")
	}
	req.System = "ordinary research"
	if _, err = meter.Complete(context.Background(), req); err == nil {
		t.Fatal("fixture did not fail")
	}
	if _, ok := meter.calls[1]["audit_messages"]; ok {
		t.Fatal("captured unrelated request")
	}
}
