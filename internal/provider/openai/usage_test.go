package openai

import (
	"encoding/json"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

// OpenAI-compatible backends fold cached tokens into prompt_tokens, while the
// rest of the stack assumes Anthropic's disjoint accounting. These cases pin
// the conversion so cache hits can never be counted twice in cost or hit-rate
// math over llm_usage.
func TestUsageReportNormalizesCacheAccounting(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantInput  int
		wantOutput int
		wantCached int
	}{
		{
			name:       "deepseek flat cache fields",
			body:       `{"prompt_tokens":2573,"completion_tokens":123,"prompt_cache_hit_tokens":2560,"prompt_cache_miss_tokens":13}`,
			wantInput:  13,
			wantOutput: 123,
			wantCached: 2560,
		},
		{
			name:       "openai nested cache details",
			body:       `{"prompt_tokens":4000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":3072}}`,
			wantInput:  928,
			wantOutput: 50,
			wantCached: 3072,
		},
		{
			name:       "backend without caching is unchanged",
			body:       `{"prompt_tokens":900,"completion_tokens":40}`,
			wantInput:  900,
			wantOutput: 40,
			wantCached: 0,
		},
		{
			name:       "cold call reports a miss, not a hit",
			body:       `{"prompt_tokens":2573,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":2573}`,
			wantInput:  2573,
			wantOutput: 10,
			wantCached: 0,
		},
		{
			name:       "nonsensical cache total falls back to raw prompt tokens",
			body:       `{"prompt_tokens":100,"completion_tokens":5,"prompt_cache_hit_tokens":900}`,
			wantInput:  100,
			wantOutput: 5,
			wantCached: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var report usageReport
			if err := json.Unmarshal([]byte(tc.body), &report); err != nil {
				t.Fatalf("unmarshal usage: %v", err)
			}
			got := report.usage()
			if got.InputTokens != tc.wantInput {
				t.Errorf("InputTokens = %d, want %d", got.InputTokens, tc.wantInput)
			}
			if got.OutputTokens != tc.wantOutput {
				t.Errorf("OutputTokens = %d, want %d", got.OutputTokens, tc.wantOutput)
			}
			if got.CacheReadTokens != tc.wantCached {
				t.Errorf("CacheReadTokens = %d, want %d", got.CacheReadTokens, tc.wantCached)
			}
			// Whatever the split, the reported prompt size must be preserved.
			if total := got.InputTokens + got.CacheReadTokens; total != report.PromptTokens {
				t.Errorf("input+cached = %d, want prompt_tokens %d", total, report.PromptTokens)
			}
		})
	}
}

// A turn that carries both tool results and text (a turn-context block, a
// final-answer directive) must still emit the tool replies immediately after
// the assistant tool_calls they answer. Anything between them fails the whole
// request with "insufficient tool messages following tool_calls message".
func TestBuildMessagesPutsToolResultsBeforeAccompanyingText(t *testing.T) {
	messages := []bs.Message{
		{Role: "user", Content: "what is the weather?"},
		{Role: "assistant", Content: []bs.ContentBlock{
			{Type: "tool_use", ID: "call_1", Name: "get_weather", Input: json.RawMessage(`{}`)},
			{Type: "tool_use", ID: "call_2", Name: "get_time", Input: json.RawMessage(`{}`)},
		}},
		{Role: "user", Content: []bs.ContentBlock{
			{Type: "tool_result", ToolUseID: "call_1", Content: "sunny"},
			{Type: "tool_result", ToolUseID: "call_2", Content: "noon"},
			{Type: "text", Text: "## Turn context\n[turn_context]\ntraces\n[/turn_context]"},
		}},
	}

	out := buildMessages("system prompt", messages, false)

	var gotRoles []string
	for _, m := range out {
		gotRoles = append(gotRoles, m.Role)
	}
	wantRoles := []string{"system", "user", "assistant", "tool", "tool", "user"}
	if len(gotRoles) != len(wantRoles) {
		t.Fatalf("role sequence = %v, want %v", gotRoles, wantRoles)
	}
	for i := range wantRoles {
		if gotRoles[i] != wantRoles[i] {
			t.Fatalf("role sequence = %v, want %v", gotRoles, wantRoles)
		}
	}

	// Every tool_call_id must be answered before any other role intervenes.
	assistantIdx := 2
	if len(out[assistantIdx].ToolCalls) != 2 {
		t.Fatalf("assistant should carry two tool calls, got %#v", out[assistantIdx].ToolCalls)
	}
	for offset, wantID := range []string{"call_1", "call_2"} {
		reply := out[assistantIdx+1+offset]
		if reply.Role != "tool" || reply.ToolCallID != wantID {
			t.Fatalf("reply %d = %#v, want tool message for %q", offset, reply, wantID)
		}
	}
	if trailing := out[len(out)-1]; trailing.Role != "user" {
		t.Fatalf("accompanying text should trail the tool replies, got %#v", trailing)
	}
}
