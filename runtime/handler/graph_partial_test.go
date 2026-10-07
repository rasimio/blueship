package handler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

func TestGraphPartialSynthesisHasNoToolsAndRejectsToolRequest(t *testing.T) {
	for _, toolRequest := range []bool{false, true} {
		provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
			var input struct {
				CurrentDatetime string `json:"current_datetime"`
			}
			if err := json.Unmarshal([]byte(core.ExtractText(core.NormalizeContent(req.Messages[0].Content))), &input); err != nil {
				t.Fatal(err)
			}
			instant, err := time.Parse(time.RFC3339, input.CurrentDatetime)
			if err != nil || time.Since(instant) < 0 || time.Since(instant) > 5*time.Second {
				t.Fatalf("missing or stale execution clock: %q (%v)", input.CurrentDatetime, err)
			}
			if len(req.Tools) != 0 || req.System != "partial instructions" {
				t.Fatal("partial synthesis allowed research")
			}
			if req.MaxTokens < 16384 {
				t.Fatal("partial report budget cannot hold a report", req.MaxTokens)
			}
			block := core.ContentBlock{Type: "text", Text: "Partial: confirmed prices; compatibility missing."}
			if toolRequest {
				block = core.ContentBlock{Type: "tool_use", Name: "fetch"}
			}
			return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "thinking", Text: "PRIVATE_TEST_MARKER"}, {Type: "redacted_thinking", Text: "PRIVATE_TEST_MARKER"}, block}}, nil
		}}
		deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-partial": "partial instructions"})
		deps.Config.Models.Primary.Name = "test-model"
		output, err := NewBackground(time.UTC, nil, nil, nil).SynthesizeGraphPartial(context.Background(), core.AgentTask{Title: "Build PC"}, []core.TaskStep{{ID: "prices", Goal: "Prices", Status: "done", Result: "Confirmed prices"}, {ID: "compatibility", Goal: "Compatibility", Status: "pending"}}, deps)
		if toolRequest {
			if err == nil || output != "" {
				t.Fatal("accepted tool request", output, err)
			}
		} else if err != nil || output != "Partial: confirmed prices; compatibility missing." {
			t.Fatal(output, err)
		}
	}
}

func TestGraphClockUsesOwnerTimezone(t *testing.T) {
	deps, _ := backgroundTestDeps(&capturingProvider{}, nil)
	deps.Config.Gateway.ResolveTimezone = func(context.Context) string { return "Asia/Tokyo" }
	clock := NewBackground(time.UTC, nil, nil, nil).graphCurrentDatetime(context.Background(), deps)
	instant, err := time.Parse(time.RFC3339, clock)
	if err != nil {
		t.Fatal(err)
	}
	_, offset := instant.Zone()
	if offset != 9*60*60 {
		t.Fatalf("wrong owner timezone: %s", clock)
	}
}

func TestGraphPartialRetainsUnverifiedReadCandidate(t *testing.T) {
	for _, checkpoint := range []string{`{"candidate_ready":true,"output":"Six priced items","review_reason":"Over budget"}`, `{"candidate_ready":false,"output":"INTERNAL repair","last_candidate":"Six priced items","last_candidate_review":"Over budget"}`} {
		for _, truncated := range []bool{false, true} {
			provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
				var input struct {
					Confirmed map[string]string `json:"confirmed_results"`
					Drafts    map[string]struct {
						Draft  string `json:"draft"`
						Review string `json:"review_feedback"`
					} `json:"unverified_drafts"`
				}
				if err := json.Unmarshal([]byte(core.ExtractText(core.NormalizeContent(req.Messages[0].Content))), &input); err != nil {
					t.Fatal(err)
				}
				if len(input.Confirmed) != 0 || len(input.Drafts) != 1 || input.Drafts["research"].Draft != "Six priced items" || input.Drafts["research"].Review != "Over budget" {
					t.Fatal(input)
				}
				stop := "end_turn"
				if truncated {
					stop = "max_tokens"
				}
				return &core.CompletionResponse{StopReason: stop, Content: []core.ContentBlock{{Type: "text", Text: "Unverified findings; over budget."}}}, nil
			}}
			deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-partial": "partial instructions"})
			deps.Config.Models.Primary.Name = "test-model"
			cp := json.RawMessage(checkpoint)
			steps := []core.TaskStep{{ID: "research", Kind: "read", Status: "blocked", Checkpoint: cp}, {ID: "action", Kind: "action", Status: "blocked", Checkpoint: cp}, {ID: "inflight", Kind: "read", Status: "running", Checkpoint: json.RawMessage(`{"output":"INTERNAL"}`)}}
			output, err := NewBackground(time.UTC, nil, nil, nil).SynthesizeGraphPartial(context.Background(), core.AgentTask{Title: "Build"}, steps, deps)
			if truncated {
				if err == nil || output != "" {
					t.Fatal("truncated synthesis accepted", output, err)
				}
			} else if err != nil || output != "Unverified findings; over budget." {
				t.Fatal(output, err)
			}
		}
	}

}
