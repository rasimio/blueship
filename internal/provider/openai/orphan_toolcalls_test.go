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

// assertToolInvariant checks what the backend checks: every tool_call is
// answered by the tool message in the matching position right behind its
// assistant message, and no tool message answers a call that is not there.
func assertToolInvariant(t *testing.T, msgs []chatMessage) {
	t.Helper()
	for i, msg := range msgs {
		if msg.Role == "tool" {
			continue
		}
		for offset, call := range msg.ToolCalls {
			if call.ID == "" {
				t.Fatalf("message %d carries a call with no id: %+v", i, call)
			}
			reply := i + 1 + offset
			if reply >= len(msgs) || msgs[reply].Role != "tool" || msgs[reply].ToolCallID != call.ID {
				t.Fatalf("call %q at message %d is not answered in position %d: %+v", call.ID, i, reply, msgs)
			}
		}
	}
	for i, msg := range msgs {
		if msg.Role != "tool" {
			continue
		}
		answers := false
		for back := i - 1; back >= 0 && !answers; back-- {
			if msgs[back].Role == "tool" {
				continue
			}
			for _, call := range msgs[back].ToolCalls {
				if call.ID == msg.ToolCallID {
					answers = true
				}
			}
			break
		}
		if !answers {
			t.Fatalf("tool message %d answers no preceding call: %+v", i, msg)
		}
	}
}

// A message landing between a call and its results — a barge-in, a turn the
// window reassembled out of order — leaves the call answered somewhere but not
// where the backend looks. Position is the invariant, not mere presence.
func TestBuildMessagesDropsACallItsReplyNoLongerFollows(t *testing.T) {
	out := buildMessages("", []bs.Message{
		{Role: "user", Content: "вопрос"},
		{Role: "assistant", Content: []bs.ContentBlock{
			{Type: "tool_use", ID: "call_1", Name: "search"},
		}},
		{Role: "user", Content: "а, забудь"},
		{Role: "user", Content: []bs.ContentBlock{
			{Type: "tool_result", ToolUseID: "call_1", Content: "нашлось"},
		}},
	}, false)

	assertToolInvariant(t, out)
	for _, m := range out {
		if len(m.ToolCalls) > 0 || m.Role == "tool" {
			t.Fatalf("the separated pair should have gone entirely, got %+v", m)
		}
	}
}

// A window that opens mid-round starts on results whose call was left behind.
// The backend rejects a tool message that answers nothing just as hard as an
// unanswered call.
func TestBuildMessagesDropsARepliesWithoutTheirCall(t *testing.T) {
	out := buildMessages("", []bs.Message{
		{Role: "user", Content: []bs.ContentBlock{
			{Type: "tool_result", ToolUseID: "call_gone", Content: "нашлось"},
			{Type: "text", Text: "и что дальше?"},
		}},
	}, false)

	assertToolInvariant(t, out)
	if len(out) != 1 || out[0].Role != "user" {
		t.Fatalf("only the user text should survive, got %+v", out)
	}
}

// A backend that streams a call without an id gives both sides an empty
// tool_call_id, which serializes away entirely — the assistant then carries a
// call no tool message can ever address.
func TestBuildMessagesDropsAnIDLessCallAndItsReply(t *testing.T) {
	out := buildMessages("", []bs.Message{
		{Role: "user", Content: "вопрос"},
		{Role: "assistant", Content: []bs.ContentBlock{
			{Type: "text", Text: "смотрю"},
			{Type: "tool_use", Name: "search"},
		}},
		{Role: "user", Content: []bs.ContentBlock{
			{Type: "tool_result", Name: "search", Content: "нашлось"},
		}},
	}, false)

	assertToolInvariant(t, out)
	for _, m := range out {
		if m.Role == "tool" {
			t.Fatalf("an unaddressable reply survived: %+v", m)
		}
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
