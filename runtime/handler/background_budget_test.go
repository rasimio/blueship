package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

func TestResearchFinalizationWindowBlocksNewResearchTools(t *testing.T) {
	provider := &capturingProvider{}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-task": "Research the mission.", "background-synthesis": "Write the result now."})
	executed := false
	deps.Registry.Register("browser_fetch", "fetch", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) { executed = true; return "page", nil })
	provider.responses = []*core.CompletionResponse{
		{StopReason: "tool_use", Content: []core.ContentBlock{{Type: "tool_use", ID: "late-fetch", Name: "browser_fetch", Input: json.RawMessage(`{}`)}}},
		{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "Result from already collected evidence."}}},
	}
	deadline := time.Now().Add(time.Minute)
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Strategy: core.StrategyDirect, Title: "research", CreatedAt: time.Now().Add(-29 * time.Minute), Deadline: &deadline, MaxIterations: 20, Config: json.RawMessage(`{"skip_reflex":true}`)}
	result, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps)
	if err != nil || !result.Done || executed {
		t.Fatalf("result=%+v executed=%v err=%v", result, executed, err)
	}
	for _, req := range provider.requests {
		if len(req.Tools) > 0 {
			t.Fatal("new tools exposed in finalization reserve")
		}
	}
}

func TestResearchContextPreparedOnceAcrossIterations(t *testing.T) {
	provider := &capturingProvider{respond: func(core.CompletionRequest) (*core.CompletionResponse, error) {
		return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "Found another useful fact. [CONTINUE]"}}}, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-task": "Research the mission."})
	prepared := 0
	deps.ContextInjector = func(context.Context, string, string, string) string { prepared++; return "USER-PREFERS-QUIET-PC" }
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Strategy: core.StrategyDirect, Title: "research", MaxIterations: 10}
	handler := NewBackground(time.UTC, nil, nil, nil)
	first, err := handler.Run(context.Background(), task, deps)
	if err != nil {
		t.Fatal(err)
	}
	task.Iteration, task.Progress = 1, first.Progress
	if _, err := handler.Run(context.Background(), task, deps); err != nil {
		t.Fatal(err)
	}
	if prepared != 1 {
		t.Fatalf("personal context prepared %d times", prepared)
	}
	if !strings.Contains(string(first.Progress), "USER-PREFERS-QUIET-PC") {
		t.Fatal("task lost user constraint snapshot")
	}
}
