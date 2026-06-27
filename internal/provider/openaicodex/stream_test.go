package openaicodex

import (
	"encoding/json"
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestParseSSEStreamConvertsXMLToolUseText(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"delta":"<tool_use name=\"memory_search\">\n  <arguments>{\"query\":\"workout\",\"limit\":10}</arguments>\n</tool_use>"}`,
		`event: response.output_item.done`,
		`data: {"item":{"type":"message"}}`,
		`event: response.completed`,
		`data: {"response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":5}}}`,
		``,
	}, "\n")

	var streamedText strings.Builder
	var toolName string
	var toolInput json.RawMessage
	resp, err := parseSSEStream(strings.NewReader(sse), &bs.StreamCallbacks{
		OnText: func(delta string) {
			streamedText.WriteString(delta)
		},
		OnToolUse: func(_ string, name string, input json.RawMessage) {
			toolName = name
			toolInput = input
		},
	}, map[string]bool{"memory_search": true})
	if err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}
	if got := streamedText.String(); got != "" {
		t.Fatalf("streamed XML as text: %q", got)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("stop reason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" {
		t.Fatalf("content = %#v, want one tool_use block", resp.Content)
	}
	if resp.Content[0].Name != "memory_search" || toolName != "memory_search" {
		t.Fatalf("tool name content=%q callback=%q", resp.Content[0].Name, toolName)
	}
	if string(resp.Content[0].Input) != `{"query":"workout","limit":10}` {
		t.Fatalf("content input = %s", resp.Content[0].Input)
	}
	if string(toolInput) != `{"query":"workout","limit":10}` {
		t.Fatalf("callback input = %s", toolInput)
	}
}

func TestParseSSEStreamDropsXMLToolResultLeak(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"delta":"<tool_use name=\"memory_search\"><arguments>{\"query\":\"\"}</arguments></tool_use><tool_result name=\"memory_search\"><result>{'results':[{'secret':'x'}]}</result></tool_result>visible tail"}`,
		`event: response.output_item.done`,
		`data: {"item":{"type":"message"}}`,
		`event: response.completed`,
		`data: {"response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":5}}}`,
		``,
	}, "\n")

	var streamedText strings.Builder
	resp, err := parseSSEStream(strings.NewReader(sse), &bs.StreamCallbacks{
		OnText: func(delta string) {
			streamedText.WriteString(delta)
		},
	}, map[string]bool{"memory_search": true})
	if err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}
	if got := streamedText.String(); strings.Contains(got, "tool_result") || strings.Contains(got, "secret") || strings.Contains(got, "visible tail") {
		t.Fatalf("leaked XML/fake continuation into stream: %q", got)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("stop reason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" {
		t.Fatalf("content = %#v, want one tool_use block", resp.Content)
	}
	if text := bs.ExtractText(resp.Content); text != "" {
		t.Fatalf("leaked text into content: %q", text)
	}
}

func TestParseSSEStreamDoesNotExecuteXMLToolWhenToolWasNotAdvertised(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"delta":"before <tool_use name=\"memory_search\"><arguments>{\"query\":\"x\"}</arguments></tool_use> after"}`,
		`event: response.output_item.done`,
		`data: {"item":{"type":"message"}}`,
		`event: response.completed`,
		`data: {"response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":5}}}`,
		``,
	}, "\n")

	var toolCalled bool
	resp, err := parseSSEStream(strings.NewReader(sse), &bs.StreamCallbacks{
		OnToolUse: func(_ string, _ string, _ json.RawMessage) {
			toolCalled = true
		},
	}, nil)
	if err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}
	if toolCalled {
		t.Fatalf("converted XML tool call even though no tools were advertised")
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if text := bs.ExtractText(resp.Content); text != "before  after" {
		t.Fatalf("content text = %q", text)
	}
}

func TestParseSSEStreamPreservesNormalText(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"delta":"hello "}`,
		`event: response.output_text.delta`,
		`data: {"delta":"world"}`,
		`event: response.output_item.done`,
		`data: {"item":{"type":"message"}}`,
		`event: response.completed`,
		`data: {"response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`,
		``,
	}, "\n")

	var streamedText strings.Builder
	resp, err := parseSSEStream(strings.NewReader(sse), &bs.StreamCallbacks{
		OnText: func(delta string) {
			streamedText.WriteString(delta)
		},
	}, map[string]bool{"memory_search": true})
	if err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}
	if got := streamedText.String(); got != "hello world" {
		t.Fatalf("streamed text = %q", got)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if text := bs.ExtractText(resp.Content); text != "hello world" {
		t.Fatalf("content text = %q", text)
	}
}
