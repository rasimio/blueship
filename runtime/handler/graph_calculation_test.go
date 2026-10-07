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

func TestGraphCalculationsExactDecimal(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"No calculation", "No calculation"},
		{"Total {{calc: 18597.60+16368+47799+56499+17737.20+20389+11670+4290; 2}} RSD", "Total 193349.80 RSD"},
		{"{{calc: 193349.80 / 117.4255; 1}} EUR", "1646.6 EUR"},
		{"{{calc: (193349.80-17737.20+20640)/117.4255; 1}} EUR", "1671.3 EUR"},
		{"{{calc: 0.1+0.2; 2}} {{calc: -1.005; 2}}", "0.30 -1.01"},
		{"{{calc: 010+2; 0}}", "12"},
	} {
		got, err := renderGraphCalculations(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("%q: %q %v, want %q", tc.input, got, err, tc.want)
		}
	}
}

func TestGraphCalculationsRejectInvalidOrUnboundedInput(t *testing.T) {
	for _, input := range []string{
		"{{calc: 1/0; 2}}", "{{calc: 1//2; 0}}", "{{calc: 1/*2*/+3; 0}}", "{{calc: 10; 7}}", "{{calc: 10; -1}}", "{{calc: 10}}",
		"{{calc: 1e999999; 0}}", "{{calc: 0x10; 0}}", "{{calc: system(1); 0}}",
		"{{calc: 1,005; 2}}", "{{calc: 1<<2; 0}}", "{{calc: 10%3; 0}}",
		"{{calc: 1; 2", "{{calc: " + strings.Repeat("1+", 150) + "1; 0}}",
		strings.Repeat("{{calc: 1; 0}}", 129),
	} {
		if got, err := renderGraphCalculations(input); err == nil || got != "" {
			t.Fatalf("accepted %q: %q %v", input, got, err)
		}
	}
}

func TestGraphStepReviewsAndPersistsCalculatedOutput(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		candidate := "Total {{calc: 193349.80/117.4255; 1}} EUR"
		if invalid {
			candidate = "Total {{calc: 1/0; 1}} EUR"
		}
		reviews := 0
		provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
			text := candidate
			if req.System == "review instructions" {
				reviews++
				input := core.ExtractText(core.NormalizeContent(req.Messages[0].Content))
				if !strings.Contains(input, "Total 1646.6 EUR") || strings.Contains(input, "{{calc:") {
					t.Fatal("review received uncomputed text", input)
				}
				text = `{"accepted":true,"reason":"verified"}`
			}
			return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: text}}}, nil
		}}
		deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute", "background-graph-review": "review instructions"})
		deps.Config.Models.Primary.Name = "test-model"
		var saved graphStepCheckpoint
		result, accepted, err := NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(context.Background(), core.AgentTask{ID: uuid.New(), UserID: uuid.New(), ExecutorVersion: 2}, core.TaskStep{ID: "read", Kind: "read"}, nil, deps, func(_ context.Context, raw json.RawMessage) error { return json.Unmarshal(raw, &saved) })
		if err != nil {
			t.Fatal(err)
		}
		if invalid {
			if accepted || reviews != 0 || saved.Phase != "rejected" || !strings.Contains(saved.ReviewReason, "divides by zero") {
				t.Fatal("invalid calculation reached review", accepted, reviews, saved)
			}
		} else if !accepted || reviews != 1 || result.Output != "Total 1646.6 EUR" || saved.Output != result.Output {
			t.Fatal(result, accepted, reviews, saved)
		}
	}
}

func TestGraphRepairRendersCalculationsBeforeReturning(t *testing.T) {
	provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
		if len(req.Tools) != 0 {
			t.Fatal("repair exposed tools")
		}
		return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"edits":[{"before":"1646.5","after":"{{calc: 193349.80/117.4255; 1}}"}]}`}}}, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-repair": "repair"})
	deps.Config.Models.Primary.Name = "test-model"
	got, err := NewBackground(time.UTC, nil, nil, nil).RepairGraphResult(context.Background(), core.AgentTask{ExecutorVersion: 2}, nil, deps, "Total 1646.5 EUR", "Correct rounding")
	if err != nil || got != "Total 1646.6 EUR" {
		t.Fatal(got, err)
	}
}
