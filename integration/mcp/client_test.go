package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type functionTransport struct {
	callFunc func(method string, params any) (json.RawMessage, error)
}

func (t functionTransport) call(_ context.Context, method string, params any) (json.RawMessage, error) {
	return t.callFunc(method, params)
}

func (functionTransport) notify(context.Context, string, any) error { return nil }
func (functionTransport) close() error                              { return nil }

func TestFlattenContentLimit(t *testing.T) {
	t.Run("at limit", func(t *testing.T) {
		text, err := flattenContent([]contentBlock{{Type: "text", Text: strings.Repeat("x", maxFlattenedToolTextBytes)}})
		if err != nil || len(text) != maxFlattenedToolTextBytes {
			t.Fatalf("flattenContent() len=%d err=%v", len(text), err)
		}
	})

	t.Run("over limit", func(t *testing.T) {
		text, err := flattenContent([]contentBlock{
			{Type: "text", Text: strings.Repeat("x", maxFlattenedToolTextBytes)},
			{Type: "text", Text: "overflow"},
		})
		if err == nil || text != "" || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("flattenContent() text_len=%d err=%v", len(text), err)
		}
	})

	t.Run("ignores non-text", func(t *testing.T) {
		text, err := flattenContent([]contentBlock{{Type: "image", Text: strings.Repeat("x", maxFlattenedToolTextBytes+1)}})
		if err != nil || text != "" {
			t.Fatalf("flattenContent() text=%q err=%v", text, err)
		}
	})
}

func TestListToolsRunawayGuards(t *testing.T) {
	t.Run("repeated cursor", func(t *testing.T) {
		calls := 0
		client := &Client{t: functionTransport{callFunc: func(string, any) (json.RawMessage, error) {
			calls++
			return json.Marshal(listToolsResult{NextCursor: "loop"})
		}}}
		if _, err := client.listTools(context.Background()); err == nil || !strings.Contains(err.Error(), "repeated cursor") {
			t.Fatalf("listTools error = %v", err)
		}
		if calls != 2 {
			t.Fatalf("calls = %d, want cycle detected on second page", calls)
		}
	})

	t.Run("max pages", func(t *testing.T) {
		calls := 0
		client := &Client{t: functionTransport{callFunc: func(string, any) (json.RawMessage, error) {
			calls++
			return json.Marshal(listToolsResult{NextCursor: fmt.Sprintf("cursor-%d", calls)})
		}}}
		if _, err := client.listTools(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeded 100 pages") {
			t.Fatalf("listTools error = %v", err)
		}
		if calls != maxListToolPages {
			t.Fatalf("calls = %d, want %d", calls, maxListToolPages)
		}
	})

	t.Run("max tools is a hard error", func(t *testing.T) {
		tools := make([]ToolDef, maxListedTools+1)
		client := &Client{t: functionTransport{callFunc: func(string, any) (json.RawMessage, error) {
			return json.Marshal(listToolsResult{Tools: tools})
		}}}
		if _, err := client.listTools(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeded 500 tools") {
			t.Fatalf("listTools error = %v", err)
		}
	})
}
