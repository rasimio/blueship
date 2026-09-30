package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

// A file written into the arguments of attachment_create can outrun the
// output limit. The call then arrives cut mid-JSON under finish_reason
// "length". That is not a broken backend: the agent loop answers such a call
// with [tool_call_truncated] and the model shrinks it — but only if the
// provider hands the call back as a stump under a max_tokens stop instead of
// failing the whole turn ("incomplete tool arguments", live 2026-09-30).
func TestStreamedToolCallCutByTheOutputLimitComesBackAsAStump(t *testing.T) {
	cut := "data: {\"choices\":[{\"delta\":{\"content\":\"Собираю файл.\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"attachment_create\",\"arguments\":\"{\\\"name\\\":\\\"report.md\\\",\\\"content\\\":\\\"# Отчёт\"}}]},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(cut))
	}))
	defer srv.Close()

	var announced []string
	cb := &bs.StreamCallbacks{OnToolUse: func(_, name string, _ json.RawMessage) { announced = append(announced, name) }}
	p := NewCompatibleProvider(srv.URL, "k", 5*time.Second, nil)
	resp, err := p.StreamComplete(context.Background(), bs.CompletionRequest{Model: "m"}, cb)
	if err != nil {
		t.Fatalf("a cut call failed the turn: %v", err)
	}
	if resp.StopReason != "max_tokens" {
		t.Fatalf("StopReason = %q, want max_tokens — anything else runs the stump", resp.StopReason)
	}
	var call *bs.ContentBlock
	for i := range resp.Content {
		if resp.Content[i].Type == "tool_use" {
			call = &resp.Content[i]
		}
	}
	if call == nil || call.ID != "call_1" || call.Name != "attachment_create" || string(call.Input) != "{}" {
		t.Fatalf("stump = %+v", call)
	}
	if len(announced) != 0 {
		t.Fatalf("a call that will not run was announced: %v", announced)
	}
}

// Cut arguments under any other stop are broken, and stay an error.
func TestStreamedToolCallWithBrokenArgumentsUnderAPlainStopIsAnError(t *testing.T) {
	broken := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"run\",\"arguments\":\"{\"}}]},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(broken))
	}))
	defer srv.Close()
	p := NewCompatibleProvider(srv.URL, "k", 5*time.Second, nil)
	if _, err := p.StreamComplete(context.Background(), bs.CompletionRequest{Model: "m"}, nil); err == nil {
		t.Fatal("broken arguments under a plain stop were accepted")
	}
}

func TestCompletedToolCallCutByTheOutputLimitComesBackAsAStump(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"role":"assistant",` +
			`"tool_calls":[{"id":"c1","type":"function","function":{"name":"attachment_create","arguments":"{\"content\":\"# Отч"}}]}}],"usage":{}}`))
	}))
	defer srv.Close()
	p := NewCompatibleProvider(srv.URL, "k", 5*time.Second, nil)
	resp, err := p.Complete(context.Background(), bs.CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "max_tokens" || len(resp.Content) != 1 || string(resp.Content[0].Input) != "{}" {
		t.Fatalf("resp = %+v", resp)
	}
}
