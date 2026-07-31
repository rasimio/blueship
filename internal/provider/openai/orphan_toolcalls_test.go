package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

// A turn that ended before its tools ran leaves a call nothing will ever
// answer. The wire format rejects the whole conversation for it, on every
// later turn — so the request must reconcile calls with results itself.
func TestBuildMessagesDropsUnansweredToolCalls(t *testing.T) {
	history := []bs.Message{
		{Role: "user", Content: "вопрос"},
		{Role: "assistant", Content: []bs.ContentBlock{
			{Type: "text", Text: "смотрю"},
			{Type: "tool_use", ID: "call_ok", Name: "search"},
			{Type: "tool_use", ID: "call_orphan", Name: "search"},
		}},
		{Role: "user", Content: []bs.ContentBlock{
			{Type: "tool_result", ToolUseID: "call_ok", Content: "нашлось"},
		}},
	}
	out := buildMessages("", history, false)

	var calls []string
	for _, m := range out {
		for _, c := range m.ToolCalls {
			calls = append(calls, c.ID)
		}
	}
	if len(calls) != 1 || calls[0] != "call_ok" {
		t.Fatalf("tool_calls on the wire = %v, want only the answered one", calls)
	}
}

// An assistant turn that is nothing but an unanswered call must vanish
// entirely rather than ride along as an empty message.
func TestBuildMessagesDropsAnAssistantTurnLeftEmpty(t *testing.T) {
	out := buildMessages("", []bs.Message{
		{Role: "user", Content: "вопрос"},
		{Role: "assistant", Content: []bs.ContentBlock{
			{Type: "tool_use", ID: "call_orphan", Name: "search"},
		}},
	}, false)
	for _, m := range out {
		if m.Role == "assistant" {
			t.Fatalf("an assistant message survived with no answerable content: %+v", m)
		}
	}
}

// A model that emits calls under a plain stop reason must still be reported as
// wanting them executed. The caller branches on the stop reason alone: told
// "end_turn", it returns with the calls already persisted and no results, and
// the history is poisoned for good. Driven through a real response so the test
// fails if the reconciliation stops being wired into the provider.
func TestProviderReportsToolUseWhenTheModelCallsUnderAPlainStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant",` +
			`"content":"смотрю","tool_calls":[{"id":"c1","type":"function",` +
			`"function":{"name":"search","arguments":"{}"}}]}}],"usage":{}}`))
	}))
	defer srv.Close()

	p := NewCompatibleProvider(srv.URL, "k", 5*time.Second, nil)
	resp, err := p.Complete(context.Background(), bs.CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("StopReason = %q, want tool_use — the caller would end the turn with an unanswered call", resp.StopReason)
	}
}

func TestStopReasonFollowsTheBlocksNotTheLabel(t *testing.T) {
	withCall := []bs.ContentBlock{{Type: "text", Text: "ок"}, {Type: "tool_use", ID: "c1", Name: "search"}}
	for _, reason := range []string{"stop", "length", ""} {
		if got := stopReasonForBlocks(reason, withCall); got != "tool_use" {
			t.Errorf("stopReasonForBlocks(%q, +tool_use) = %q, want tool_use", reason, got)
		}
	}
	textOnly := []bs.ContentBlock{{Type: "text", Text: "ок"}}
	if got := stopReasonForBlocks("stop", textOnly); got != "end_turn" {
		t.Errorf("plain text turn = %q, want end_turn", got)
	}
	if got := stopReasonForBlocks("length", textOnly); got != "max_tokens" {
		t.Errorf("truncated text turn = %q, want max_tokens", got)
	}
}
