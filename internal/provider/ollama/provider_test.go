package ollama

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestBuildRequestSetsContextWindow(t *testing.T) {
	p := NewCompletionProvider("", time.Second, nil)

	req := p.buildRequest(bs.CompletionRequest{
		Model:         "gemma4:e4b",
		MaxTokens:     16,
		ContextWindow: 4096,
	}, false)

	if got := req.Options["num_ctx"]; got != 4096 {
		t.Fatalf("num_ctx = %v, want %d", got, 4096)
	}
	if got := req.Options["num_predict"]; got != 16 {
		t.Fatalf("num_predict = %v, want %d", got, 16)
	}
}

// A tool result must reach the model adjacent to the call it answers.
//
// Ollama's Gemma renderer gathers tool results only while they immediately
// follow the assistant message holding the tool calls; the first message of any
// other role ends the run, and a tool message it never reached is dropped
// instead of rendered. Emitting this turn's text before the results therefore
// removed the results from the prompt entirely — and the model, handed no data,
// answered with a confident invented number rather than admitting ignorance.
func TestBuildMessagesKeepsToolResultsAdjacentToTheCall(t *testing.T) {
	msgs := buildMessages("SYSTEM", []bs.Message{
		{Role: "assistant", Content: []bs.ContentBlock{
			{Type: "tool_use", Name: "agent_task_status", Input: json.RawMessage(`{"task_id":"d0bf9fce"}`)},
		}},
		{Role: "user", Content: []bs.ContentBlock{
			{Type: "tool_result", Content: `{"iteration":7}`},
			{Type: "text", Text: "[turn_context] … [/turn_context]"},
		}},
	})

	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	want := []string{"system", "assistant", "tool", "user"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v — a message between the call and its result hides the result", roles, want)
		}
	}
	if !strings.Contains(msgs[2].Content, `"iteration":7`) {
		t.Fatalf("tool message lost its payload: %q", msgs[2].Content)
	}
}

// Ollama reports nanoseconds; the shared Usage carries milliseconds. Dropping
// these was why a 40-second turn could only be explained by reading the
// inference server's log on the host.
func TestUsageFromCarriesTheWallClockSplit(t *testing.T) {
	got := usageFrom(chatResponse{
		PromptEvalCount:    14423,
		EvalCount:          142,
		PromptEvalDuration: 11384360000, // 11384.36 ms
		EvalDuration:       162220000,   //   162.22 ms
		LoadDuration:       3050000000,  //  3050.00 ms
	})
	if got.InputTokens != 14423 || got.OutputTokens != 142 {
		t.Fatalf("token counts lost: %+v", got)
	}
	if got.PrefillMillis != 11384 || got.DecodeMillis != 162 || got.LoadMillis != 3050 {
		t.Fatalf("timings = prefill %d decode %d load %d, want 11384/162/3050",
			got.PrefillMillis, got.DecodeMillis, got.LoadMillis)
	}
}

// A provider that reports no timings must leave them zero rather than invent
// them, so the log attribute is simply absent instead of claiming 0 ms.
func TestUsageFromLeavesTimingsZeroWhenAbsent(t *testing.T) {
	got := usageFrom(chatResponse{PromptEvalCount: 10, EvalCount: 2})
	if got.PrefillMillis != 0 || got.DecodeMillis != 0 || got.LoadMillis != 0 {
		t.Fatalf("timings invented from nothing: %+v", got)
	}
}

// Unset must stay off the wire entirely rather than serialise as a zero, which
// Ollama would read as "unload immediately" — the exact opposite of the
// default it is supposed to preserve.
func TestBuildRequestOmitsKeepAliveWhenUnset(t *testing.T) {
	p := NewCompletionProvider("", time.Second, nil)

	body, err := json.Marshal(p.buildRequest(bs.CompletionRequest{Model: "gemma4:e4b"}, false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "keep_alive") {
		t.Fatalf("keep_alive present with no setting: %s", body)
	}
}

func TestBuildRequestCarriesKeepAlive(t *testing.T) {
	for _, ka := range []any{-1, "30m"} {
		p := NewCompletionProvider("", time.Second, ka)
		req := p.buildRequest(bs.CompletionRequest{Model: "gemma4:e4b"}, false)
		if req.KeepAlive != ka {
			t.Fatalf("keep_alive = %v, want %v", req.KeepAlive, ka)
		}
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(body), `"keep_alive"`) {
			t.Fatalf("keep_alive %v did not reach the wire: %s", ka, body)
		}
	}
}
