package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

type checkpointProvider struct {
	calls int
	err   error
}

func (p *checkpointProvider) Complete(context.Context, bs.CompletionRequest) (*bs.CompletionResponse, error) {
	p.calls++
	if p.calls > 1 {
		return nil, p.err
	}
	return &bs.CompletionResponse{StopReason: "tool_use", Content: []bs.ContentBlock{
		{Type: "text", Text: "Verified the first component."},
		{Type: "tool_use", ID: "read-1", Name: "lookup", Input: json.RawMessage(`{}`)},
	}}, nil
}

func TestTrackedProviderFailurePreservesCheckpointAndReceipt(t *testing.T) {
	failure := errors.New("provider unavailable")
	provider := &checkpointProvider{err: failure}
	registry := bs.NewToolRegistry()
	registry.Register("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) { return "confirmed price", nil })
	loop := NewLoop(provider, &fakeMessageStore{}, registry, nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var checkpoints []RunCheckpoint
	result, err := loop.RunTracked(context.Background(), RunConfig{
		SessionID: "saved-session", Model: "test", MaxTurns: 3, MaxTokens: 100, MessageBudget: 6000,
		OnCheckpoint: func(_ context.Context, cp RunCheckpoint) error { checkpoints = append(checkpoints, cp); return nil },
	}, "find prices")
	if !errors.Is(err, failure) || result == nil || result.Text != "Verified the first component." || len(result.ToolTraces) != 1 {
		t.Fatalf("lost work on provider error: result=%+v err=%v", result, err)
	}
	if result.Outcome.Reason != RunStopError || result.ToolTraces[0].BlockID != "read-1" || result.ToolTraces[0].Receipt == nil {
		t.Fatalf("lost outcome or receipt: %+v", result)
	}
	if len(checkpoints) != 3 || checkpoints[1].PendingTool == nil || checkpoints[1].PendingTool.ID != "read-1" || checkpoints[2].PendingTool != nil || len(checkpoints[2].ToolTraces) != 1 {
		t.Fatalf("action checkpoints: %+v", checkpoints)
	}
}

func TestCheckpointFailurePreventsExternalAction(t *testing.T) {
	provider := &checkpointProvider{err: errors.New("unused")}
	registry := bs.NewToolRegistry()
	executed := false
	registry.Register("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) { executed = true; return "value", nil })
	loop := NewLoop(provider, &fakeMessageStore{}, registry, nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	failure := errors.New("checkpoint unavailable")
	result, err := loop.RunTracked(context.Background(), RunConfig{
		SessionID: "test", Model: "test", MaxTurns: 3, MaxTokens: 100, MessageBudget: 6000,
		OnCheckpoint: func(_ context.Context, cp RunCheckpoint) error {
			if cp.PendingTool != nil {
				return failure
			}
			return nil
		},
	}, "find prices")
	if !errors.Is(err, failure) || executed || provider.calls != 1 || result == nil {
		t.Fatalf("continued after persistence failure: executed=%v calls=%d result=%+v err=%v", executed, provider.calls, result, err)
	}
}

func TestCheckpointAfterToolSurvivesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := bs.NewToolRegistry()
	registry.Register("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) { cancel(); return "value", nil })
	provider := &checkpointProvider{err: context.Canceled}
	loop := NewLoop(provider, &fakeMessageStore{}, registry, nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	persisted := false
	_, _ = loop.RunTracked(ctx, RunConfig{
		SessionID: "test", Model: "test", MaxTurns: 3, MaxTokens: 100, MessageBudget: 6000,
		OnCheckpoint: func(cpCtx context.Context, cp RunCheckpoint) error {
			if cp.Phase == "tool_completed" {
				if cpCtx.Err() != nil {
					t.Fatal("checkpoint inherited cancellation")
				}
				if _, ok := cpCtx.Deadline(); !ok {
					t.Fatal("checkpoint context has no bound")
				}
				persisted = true
			}
			return nil
		},
	}, "find prices")
	if !persisted {
		t.Fatal("completed/uncertain action receipt was not checkpointed")
	}
}
