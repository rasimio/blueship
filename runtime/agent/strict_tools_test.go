package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestRunTrackedStrictToolsRejectsUndeclaredDispatch(t *testing.T) {
	runStrictToolDispatchTest(t, false)
}

func TestRunStreamStrictToolsRejectsUndeclaredDispatch(t *testing.T) {
	runStrictToolDispatchTest(t, true)
}

func TestRunTrackedPersistsCanonicalVisibleUserText(t *testing.T) {
	visible := "post-STT transcript"
	expanded := []bs.ContentBlock{
		{Type: "text", Text: "[reply to: large parent]\n\npost-STT transcript"},
		{
			Type: "image",
			Source: &bs.ImageSource{
				Type: "base64", MediaType: "image/png", Data: "SECRET_PARENT_BYTES",
			},
		},
	}
	store := &fakeMessageStore{}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{{
		Content:    []bs.ContentBlock{{Type: "text", Text: "reply"}},
		StopReason: "end_turn",
	}}}
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{
		Gateway: bs.GatewayConfig{MaxTurns: 1},
		Limits:  bs.LimitsConfig{MaxOutputTokens: 64},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := loop.RunTracked(context.Background(), RunConfig{
		SessionID:       "s1",
		SystemPrompt:    "system",
		Model:           "test",
		MaxTurns:        1,
		MessageBudget:   6000,
		VisibleUserText: &visible,
	}, expanded)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.appended) < 1 || store.appended[0].VisibleText == nil ||
		*store.appended[0].VisibleText != visible {
		t.Fatalf("persisted user message = %#v", store.appended)
	}
	persistedJSON, err := json.Marshal(store.appended[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persistedJSON), "SECRET_PARENT_BYTES") ||
		strings.Contains(string(persistedJSON), "large parent") {
		t.Fatalf("provider expansion leaked into durable content: %s", persistedJSON)
	}
	requestJSON, err := json.Marshal(provider.requests[0].Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(requestJSON), "SECRET_PARENT_BYTES") ||
		!strings.Contains(string(requestJSON), "large parent") {
		t.Fatalf("current provider prompt lost expanded payload: %s", requestJSON)
	}
}

func TestRunStreamTwoRepliesDoNotDuplicateParentPayload(t *testing.T) {
	const parentSecret = "PARENT_BASE64_SECRET"
	store := &fakeMessageStore{}
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		{Content: []bs.ContentBlock{{Type: "text", Text: "reply one"}}, StopReason: "end_turn"},
		{Content: []bs.ContentBlock{{Type: "text", Text: "reply two"}}, StopReason: "end_turn"},
	}}
	loop := NewLoop(provider, store, bs.NewToolRegistry(), nil, &bs.Config{
		Gateway: bs.GatewayConfig{MaxTurns: 1},
		Limits:  bs.LimitsConfig{MaxOutputTokens: 64},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	for _, visible := range []string{"first reply", "second reply"} {
		expanded := []bs.ContentBlock{
			{Type: "text", Text: "[reply to: repeated large parent]\n\n" + visible},
			{
				Type: "image",
				Source: &bs.ImageSource{
					Type: "base64", MediaType: "image/png", Data: parentSecret,
				},
			},
		}
		_, _, err := loop.RunStream(context.Background(), RunConfig{
			SessionID:       "s1",
			SystemPrompt:    "system",
			Model:           "test",
			MaxTurns:        1,
			MessageBudget:   6000,
			VisibleUserText: &visible,
		}, expanded, nil)
		if err != nil {
			t.Fatal(err)
		}
	}

	userRows := 0
	for _, message := range store.appended {
		if message.Role != "user" {
			continue
		}
		userRows++
		raw, err := json.Marshal(message.Content)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), parentSecret) ||
			strings.Contains(string(raw), "repeated large parent") {
			t.Fatalf("reply %d duplicated parent payload: %s", userRows, raw)
		}
	}
	if userRows != 2 {
		t.Fatalf("persisted user rows = %d, want 2", userRows)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	for i, request := range provider.requests {
		raw, err := json.Marshal(request.Messages)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), parentSecret) {
			t.Fatalf("provider request %d lost current parent payload: %s", i+1, raw)
		}
	}
}

func runStrictToolDispatchTest(t *testing.T, stream bool) {
	t.Helper()
	forbiddenCalled := false
	registry := bs.NewToolRegistry()
	registry.Register("allowed", "allowed", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) { return "ok", nil })
	registry.Register("forbidden", "forbidden", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) {
			forbiddenCalled = true
			return "should not run", nil
		})
	provider := &scriptedProvider{responses: []*bs.CompletionResponse{
		{
			Content: []bs.ContentBlock{{
				Type:  "tool_use",
				ID:    "call-1",
				Name:  "forbidden",
				Input: json.RawMessage(`{}`),
			}},
			StopReason: "tool_use",
		},
		{
			Content:    []bs.ContentBlock{{Type: "text", Text: "done"}},
			StopReason: "end_turn",
		},
	}}
	store := &fakeMessageStore{}
	loop := NewLoop(provider, store, registry, nil, &bs.Config{
		Gateway: bs.GatewayConfig{MaxTurns: 3},
		Limits:  bs.LimitsConfig{MaxOutputTokens: 64},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := RunConfig{
		SessionID:        "s1",
		SystemPrompt:     "system",
		Model:            "test",
		MaxTokens:        64,
		MaxTurns:         3,
		MessageBudget:    6000,
		ToolOverride:     []string{"allowed"},
		ToolboxExpansion: []string{"allowed", "forbidden"},
		StrictTools:      true,
	}

	var traces []ToolTrace
	if stream {
		var err error
		_, traces, err = loop.RunStream(context.Background(), cfg, "question", nil)
		if err != nil {
			t.Fatalf("RunStream: %v", err)
		}
	} else {
		result, err := loop.RunTracked(context.Background(), cfg, "question")
		if err != nil {
			t.Fatalf("RunTracked: %v", err)
		}
		traces = result.ToolTraces
	}
	if forbiddenCalled {
		t.Fatal("strict policy dispatched a tool outside the concrete turn set")
	}
	if len(traces) != 1 || traces[0].Name != "forbidden" || !traces[0].Error {
		t.Fatalf("traces = %#v, want one denied forbidden call", traces)
	}
	if len(provider.requests) == 0 || len(provider.requests[0].Tools) != 1 ||
		provider.requests[0].Tools[0].Name != "allowed" {
		t.Fatalf("strict request tools = %#v, want exactly allowed and no toolbox", provider.requests[0].Tools)
	}
}
