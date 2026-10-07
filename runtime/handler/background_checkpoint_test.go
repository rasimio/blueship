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

func TestBackgroundProviderErrorReturnsPartialIteration(t *testing.T) {
	failure := errors.New("provider unavailable after successful tool")
	calls := 0
	provider := &capturingProvider{respond: func(core.CompletionRequest) (*core.CompletionResponse, error) {
		calls++
		if calls > 1 {
			return nil, failure
		}
		return &core.CompletionResponse{StopReason: "tool_use", Content: []core.ContentBlock{
			{Type: "text", Text: "A confirmed finding."},
			{Type: "tool_use", ID: "read-1", Name: "lookup", Input: json.RawMessage(`{}`)},
		}}, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-task": "Investigate the task."})
	deps.Registry.Register("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) { return "confirmed", nil })
	result, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), core.AgentTask{
		ID: uuid.New(), UserID: uuid.New(), Strategy: core.StrategyDirect,
		Title: "find prices", MaxIterations: 3, Config: json.RawMessage(`{"skip_reflex":true}`),
	}, deps)
	if !errors.Is(err, failure) || result.Done || result.IsFinal || result.Notify != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(result.Output, "confirmed finding") || !strings.Contains(string(result.Progress), "sess-test") || !strings.Contains(string(result.ToolCallsJSON), "read-1") {
		t.Fatalf("lost partial iteration: %+v", result)
	}
}
