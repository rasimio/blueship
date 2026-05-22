package anthropic

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/core"
)

// sseEvents joins SSE data lines into a stream body — events are separated by
// blank lines, as the wire format requires.
func sseEvents(lines ...string) string {
	return strings.Join(lines, "\n\n") + "\n\n"
}

// streamTestProvider returns a Provider whose HTTP transport replies with the
// given canned SSE body.
func streamTestProvider(sse string) *Provider {
	p := NewProvider("test-key", time.Second, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(sse)),
				Header:     make(http.Header),
			}, nil
		}),
	}
	return p
}

func TestStreamCompleteText(t *testing.T) {
	sse := sseEvents(
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":12,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Привет"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", напарник"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
		`data: {"type":"message_stop"}`,
	)

	var got []string
	resp, err := streamTestProvider(sse).StreamComplete(
		context.Background(),
		bs.CompletionRequest{Model: "claude-sonnet-4-6", MaxTokens: 128},
		func(s string) { got = append(got, s) },
	)
	if err != nil {
		t.Fatalf("StreamComplete error: %v", err)
	}
	if len(got) != 2 || got[0] != "Привет" || got[1] != ", напарник" {
		t.Fatalf("unexpected onText chunks: %#v", got)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "Привет, напарник" {
		t.Fatalf("unexpected content: %#v", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("unexpected stop reason: %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("unexpected usage: %+v", resp.Usage)
	}
}

func TestStreamCompleteToolUse(t *testing.T) {
	sse := sseEvents(
		`data: {"type":"message_start","message":{"id":"msg_2","role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":20,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Так, гляну секунду"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"escalate","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"reason\":\"web"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" search\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":15}}`,
		`data: {"type":"message_stop"}`,
	)

	var got []string
	resp, err := streamTestProvider(sse).StreamComplete(
		context.Background(),
		bs.CompletionRequest{Model: "claude-sonnet-4-6", MaxTokens: 128},
		func(s string) { got = append(got, s) },
	)
	if err != nil {
		t.Fatalf("StreamComplete error: %v", err)
	}
	if len(got) != 1 || got[0] != "Так, гляну секунду" {
		t.Fatalf("unexpected onText chunks: %#v", got)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %#v", resp.Content)
	}
	if resp.Content[0].Type != "text" || resp.Content[0].Text != "Так, гляну секунду" {
		t.Fatalf("unexpected text block: %#v", resp.Content[0])
	}
	tool := resp.Content[1]
	if tool.Type != "tool_use" || tool.ID != "toolu_1" || tool.Name != "escalate" {
		t.Fatalf("unexpected tool block: %#v", tool)
	}
	if string(tool.Input) != `{"reason":"web search"}` {
		t.Fatalf("unexpected tool input: %s", tool.Input)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("unexpected stop reason: %q", resp.StopReason)
	}
}

func TestStreamCompleteSkipsThinking(t *testing.T) {
	sse := sseEvents(
		`data: {"type":"message_start","message":{"id":"msg_3","role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":5,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"secret reasoning"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"visible answer"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		`data: {"type":"message_stop"}`,
	)

	var got []string
	resp, err := streamTestProvider(sse).StreamComplete(
		context.Background(),
		bs.CompletionRequest{Model: "claude-sonnet-4-6", MaxTokens: 128, ThinkingBudget: 1024},
		func(s string) { got = append(got, s) },
	)
	if err != nil {
		t.Fatalf("StreamComplete error: %v", err)
	}
	if len(got) != 1 || got[0] != "visible answer" {
		t.Fatalf("thinking leaked into onText: %#v", got)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "visible answer" {
		t.Fatalf("thinking leaked into content: %#v", resp.Content)
	}
}

func TestParseAnthropicStreamCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sse := sseEvents(
		`data: {"type":"message_start","message":{"id":"m","role":"assistant","usage":{"input_tokens":1,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
	)

	_, _, err := parseAnthropicStream(ctx, strings.NewReader(sse), nil)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
