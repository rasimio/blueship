package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

func outcomeText(stop, text string) *bs.CompletionResponse {
	return &bs.CompletionResponse{StopReason: stop, Content: []bs.ContentBlock{{Type: "text", Text: text}}, Usage: bs.Usage{InputTokens: 10, OutputTokens: 10}}
}

func outcomeTool(id string) *bs.CompletionResponse {
	return &bs.CompletionResponse{StopReason: "tool_use", Content: []bs.ContentBlock{{Type: "tool_use", ID: id, Name: "lookup", Input: json.RawMessage(`{}`)}}}
}

func TestRunOutcomeDistinguishesModelFinishFromHarnessLimits(t *testing.T) {
	tests := []struct {
		name, stop string
		responses  func() []*bs.CompletionResponse
		maxTools   int
		want       RunStopReason
		turns      int
		toolTurns  int
	}{
		{"finished", "end_turn", func() []*bs.CompletionResponse {
			return []*bs.CompletionResponse{outcomeText("end_turn", "same visible answer")}
		}, 5, RunStopCompleted, 1, 0},
		{"tool budget despite normal final text", "end_turn", func() []*bs.CompletionResponse {
			return []*bs.CompletionResponse{outcomeTool("one"), outcomeText("end_turn", "same visible answer")}
		}, 1, RunStopToolBudget, 2, 1},
		{"turn limit with nil error and tool trace", "tool_use", func() []*bs.CompletionResponse { return []*bs.CompletionResponse{outcomeTool("one")} }, 5, RunStopTurnLimit, 1, 1},
		{"output cap after automatic continuation", "max_tokens", func() []*bs.CompletionResponse {
			return []*bs.CompletionResponse{outcomeText("max_tokens", "first part"), outcomeText("max_tokens", "second part")}
		}, 5, RunStopOutputLimit, 2, 0},
		{"refusal", "refusal", func() []*bs.CompletionResponse {
			return []*bs.CompletionResponse{outcomeText("refusal", "same visible answer")}
		}, 5, RunStopRefusal, 1, 0},
		{"empty fallback", "end_turn", func() []*bs.CompletionResponse { return []*bs.CompletionResponse{{StopReason: "end_turn"}} }, 5, RunStopEmptyOutput, 1, 0},
		{"unrecognized provider stop", "unrecognized", func() []*bs.CompletionResponse {
			return []*bs.CompletionResponse{outcomeText("unrecognized", "same visible answer")}
		}, 5, RunStopProviderStop, 1, 0},
		{"forced final cannot execute more tools", "tool_use", func() []*bs.CompletionResponse {
			return []*bs.CompletionResponse{outcomeTool("one"), outcomeTool("forbidden-extra")}
		}, 1, RunStopToolBudget, 2, 2},
	}
	for _, stream := range []bool{false, true} {
		for _, tt := range tests {
			t.Run(map[bool]string{false: "tracked", true: "stream"}[stream]+"/"+tt.name, func(t *testing.T) {
				provider := &scriptedProvider{responses: tt.responses()}
				store := &fakeMessageStore{}
				registry := bs.NewToolRegistry()
				executed := 0
				registry.Register("lookup", "lookup", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (any, error) { executed++; return "evidence", nil })
				loop := NewLoop(provider, store, registry, nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
				var outcomes []RunOutcome
				cfg := RunConfig{SessionID: "test", Model: "test", MaxTokens: 64, MaxTurns: 1, MaxToolTurns: tt.maxTools, MessageBudget: 6000, AutomaticContinuation: true, EmptyVisibleFallback: "empty", OnOutcome: func(outcome RunOutcome) { outcomes = append(outcomes, outcome) }}
				if stream {
					if _, _, err := loop.RunStream(context.Background(), cfg, "question", nil); err != nil {
						t.Fatal(err)
					}
				} else {
					result, err := loop.RunTracked(context.Background(), cfg, "question")
					if err != nil {
						t.Fatal(err)
					}
					if len(outcomes) != 1 || result.Outcome != outcomes[0] {
						t.Fatalf("returned outcome differs from callback: %#v %#v", result, outcomes)
					}
				}
				if len(outcomes) != 1 {
					t.Fatalf("callback count: %d", len(outcomes))
				}
				got := outcomes[0]
				if got.Reason != tt.want || got.Turns != tt.turns || got.ToolTurns != tt.toolTurns || got.ProviderStopReason != tt.stop {
					t.Fatalf("outcome=%+v, want reason=%s turns=%d tools=%d stop=%s", got, tt.want, tt.turns, tt.toolTurns, tt.stop)
				}
				if tt.want == RunStopToolBudget {
					if executed != 1 || len(provider.requests) != 2 || len(provider.requests[1].Tools) != 0 {
						t.Fatalf("bounded tool dispatch failed: executed=%d requests=%d", executed, len(provider.requests))
					}
					last, _ := json.Marshal(provider.requests[1].Messages)
					if !strings.Contains(string(last), "run_checkpoint") || strings.Contains(string(last), "next user message") {
						t.Fatalf("durable host was told to await manual continuation: %s", last)
					}
				}
			})
		}
	}
}

type failedOutcomeProvider struct{ err error }

func (p failedOutcomeProvider) Complete(context.Context, bs.CompletionRequest) (*bs.CompletionResponse, error) {
	return nil, p.err
}
func (p failedOutcomeProvider) StreamComplete(context.Context, bs.CompletionRequest, *bs.StreamCallbacks) (*bs.CompletionResponse, error) {
	return nil, p.err
}

func TestRunOutcomeReportsErrorsCancellationAndBatchFallbackOnce(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			err    error
			reason RunStopReason
		}{{errors.New("provider unavailable"), RunStopError}, {context.DeadlineExceeded, RunStopCancelled}} {
			loop := NewLoop(failedOutcomeProvider{tc.err}, &fakeMessageStore{}, bs.NewToolRegistry(), nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			count := 0
			cfg := RunConfig{SessionID: "s", Model: "test", MaxTokens: 64, MaxTurns: 1, MessageBudget: 6000, OnOutcome: func(outcome RunOutcome) {
				count++
				if outcome.Reason != tc.reason {
					t.Fatalf("outcome=%+v want %s", outcome, tc.reason)
				}
			}}
			var err error
			if stream {
				_, _, err = loop.RunStream(context.Background(), cfg, "test", nil)
			} else {
				_, err = loop.Run(context.Background(), cfg, "test")
			}
			if err == nil || count != 1 {
				t.Fatalf("error=%v callbacks=%d", err, count)
			}
		}
	}
	provider := &batchOnlyProvider{response: outcomeText("end_turn", "done")}
	loop := NewLoop(provider, &fakeMessageStore{}, bs.NewToolRegistry(), nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	count := 0
	_, _, err := loop.RunStream(context.Background(), RunConfig{SessionID: "s", Model: "test", MaxTokens: 64, MaxTurns: 1, MessageBudget: 6000, OnOutcome: func(outcome RunOutcome) {
		count++
		if outcome.Reason != RunStopCompleted {
			t.Fatal(outcome)
		}
	}}, "test", nil)
	if err != nil || count != 1 {
		t.Fatalf("batch fallback error=%v callbacks=%d", err, count)
	}
}

func TestLegacyFinalDirectiveKeepsManualContinuationDefault(t *testing.T) {
	message := bs.Message{Role: "user", Content: "tool results"}
	appendFinalAnswerDirective(&message)
	encoded, _ := json.Marshal(message)
	if !strings.Contains(string(encoded), "next user message") {
		t.Fatal("default caller contract changed")
	}
}
