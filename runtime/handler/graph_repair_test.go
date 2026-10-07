package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

func TestGraphRepairUsesOnlyConfirmedInputsWithoutTools(t *testing.T) {
	for _, mode := range []string{"text", "tool_use", "empty", "nil", "max_tokens"} {
		t.Run(mode, func(t *testing.T) {
			provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
				if req.MaxTokens < 16384 {
					t.Fatal("repair report budget too small", req.MaxTokens)
				}
				if len(req.Tools) != 0 || req.System != "repair instructions" {
					t.Fatal("repair allowed external work", req)
				}
				var input struct {
					Confirmed map[string]string `json:"confirmed_results"`
					Draft     string            `json:"draft"`
					Feedback  string            `json:"feedback"`
				}
				if err := json.Unmarshal([]byte(core.ExtractText(core.NormalizeContent(req.Messages[0].Content))), &input); err != nil {
					t.Fatal(err)
				}
				if len(input.Confirmed) != 1 || input.Confirmed["read"] != "Prices\nConfirmed prices" || input.Draft != "Draft" || input.Feedback != "Include total" {
					t.Fatal("repair lost feedback or included unconfirmed evidence", input)
				}
				if mode == "nil" {
					return nil, nil
				}
				if mode == "max_tokens" {
					return &core.CompletionResponse{StopReason: "max_tokens", Content: []core.ContentBlock{{Type: "text", Text: "Incomplete repair"}}}, nil
				}
				block := core.ContentBlock{Type: mode, Text: `{"edits":[{"before":"Draft","after":"Revised report"}]}`}
				if mode == "empty" {
					block = core.ContentBlock{Type: "text", Text: " "}
				}
				return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "thinking", Text: "PRIVATE_TEST_MARKER"}, {Type: "redacted_thinking", Text: "PRIVATE_TEST_MARKER"}, block}}, nil
			}}
			deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-repair": "repair instructions"})
			deps.Config.Models.Primary.Name = "test-model"
			steps := []core.TaskStep{
				{ID: "read", Kind: "read", Goal: "Prices", Status: "done", Result: "Confirmed prices"},
				{ID: "unfinished", Kind: "read", Status: "pending", Result: "Unconfirmed"},
				{ID: "final", Kind: "finalize", Status: "done", Result: "Rejected report"},
			}
			body, err := NewBackground(time.UTC, nil, nil, nil).RepairGraphResult(context.Background(), core.AgentTask{Title: "Build PC"}, steps, deps, "Draft", "Include total")
			if mode == "text" {
				if err != nil || body != "Revised report" {
					t.Fatal(body, err)
				}
			} else if err == nil || body != "" {
				t.Fatal("accepted invalid repair", body, err)
			}
		})
	}
}

func TestApplyGraphRepairEdits(t *testing.T) {
	for _, tc := range []struct {
		name, draft, patch, want string
	}{
		{"unicode and preserved citations", "Цена 100 ₽ [1]. Итог 200 ₽ [2].", `{"edits":[{"before":"200 ₽","after":"150 ₽"},{"before":"100 ₽","after":"50 ₽"}]}`, "Цена 50 ₽ [1]. Итог 150 ₽ [2]."},
		{"original positions", "alpha beta", `{"edits":[{"before":"alpha","after":"beta"},{"before":"beta","after":"gamma"}]}`, "beta gamma"},
		{"optional deletion", "keep optional", `{"edits":[{"before":" optional","after":""}]}`, "keep"},
		{"repeated passage", "a a", `{"edits":[{"before":"a","after":"b"}]}`, ""},
		{"overlapping occurrences", "aaa", `{"edits":[{"before":"aa","after":"b"}]}`, ""},
		{"overlapping edits", "abcdef", `{"edits":[{"before":"abc","after":"x"},{"before":"bcde","after":"y"}]}`, ""},
		{"duplicate edit", "abc", `{"edits":[{"before":"a","after":"x"},{"before":"a","after":"y"}]}`, ""},
		{"missing passage", "abc", `{"edits":[{"before":"z","after":"x"}]}`, ""},
		{"empty match", "abc", `{"edits":[{"before":"","after":"x"}]}`, ""},
		{"missing after", "abc", `{"edits":[{"before":"a"}]}`, ""},
		{"null after", "abc", `{"edits":[{"before":"a","after":null}]}`, ""},
		{"optional reason", "abc", `{"edits":[{"before":"a","after":"x","reason":""}]}`, "xbc"},
		{"reason cannot replace edit", "abc", `{"edits":[{"before":"a","reason":"Use x"}]}`, ""},
		{"unknown field", "abc", `{"edits":[{"before":"a","after":"x","extra":true}]}`, ""},
		{"trailing object", "abc", `{"edits":[{"before":"a","after":"x"}]} {}`, ""},
		{"trailing junk", "abc", `{"edits":[{"before":"a","after":"x"}]} junk`, ""},
		{"no edits", "abc", `{"edits":[]}`, ""},
		{"no change", "abc", `{"edits":[{"before":"a","after":"a"}]}`, ""},
		{"empty report", "abc", `{"edits":[{"before":"abc","after":" "}]}`, ""},
		{"full report rejected", "abc", "rewritten report", ""},
		{"atomic rejection", "abc", `{"edits":[{"before":"a","after":"x"},{"before":"missing","after":"y"}]}`, ""},
		{"too many edits", "abc", `{"edits":[` + strings.Repeat(`{"before":"a","after":"b"},`, 20) + `{"before":"a","after":"b"}]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyGraphRepairEdits(tc.draft, tc.patch)
			if tc.want == "" {
				if err == nil || got != "" {
					t.Fatalf("invalid patch accepted: %q, %v", got, err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
