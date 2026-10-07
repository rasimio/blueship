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

func TestGraphFinalRepairReadsOnlySavedTaskEvidence(t *testing.T) {
	for _, mode := range []string{"normal", "forbidden", "limit", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), SoulID: uuid.New(), ExecutorVersion: 2}
			calls, reads, external := 0, 0, 0
			provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
				calls++
				for _, def := range req.Tools {
					if def.Name != "saved_fetch" {
						t.Fatal("external definition exposed", def.Name)
					}
				}
				if calls == 1 || (mode == "limit" && calls == 2) {
					name := "saved_fetch"
					if mode == "forbidden" {
						name = "write"
					}
					return &core.CompletionResponse{StopReason: "tool_use", Content: []core.ContentBlock{{Type: "text", Text: "Intermediate narration is not a JSON patch."}, {Type: "tool_use", ID: "read", Name: name, Input: json.RawMessage(`{}`)}}}, nil
				}
				if mode != "forbidden" {
					raw, marshalErr := json.Marshal(req.Messages)
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					found := strings.Contains(string(raw), "microATX supported")
					if !found {
						t.Fatal("saved evidence missing from repair context")
					}
				}
				stop := "end_turn"
				if mode == "truncated" {
					stop = "max_tokens"
				}
				return &core.CompletionResponse{StopReason: stop, Content: []core.ContentBlock{{Type: "text", Text: `{"edits":[{"before":"Unknown support","after":"microATX supported"}]}`}}}, nil
			}}
			deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-repair": "repair using saved evidence"})
			deps.Config.Models.Primary.Name = "test-model"
			deps.Registry.Register("saved_fetch", "source", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) { external++; return "NETWORK", nil })
			deps.Registry.Register("write", "effect", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) { external++; return "MUTATION", nil })
			if err := deps.Registry.MarkReadOnly("saved_fetch"); err != nil {
				t.Fatal(err)
			}
			if err := deps.Registry.RegisterEvidenceReader("saved_fetch", func(ctx context.Context, _ json.RawMessage) (any, error) {
				id, ok := core.TaskIDFromContext(ctx)
				if !ok || id != task.ID || core.SoulIDFromContext(ctx) != task.SoulID {
					t.Fatal("repair lost task scope")
				}
				reads++
				return "microATX supported", nil
			}); err != nil {
				t.Fatal(err)
			}
			got, err := NewBackground(time.UTC, nil, nil, nil).RepairGraphResult(context.Background(), task, nil, deps, "Unknown support", "Find saved support")
			if external != 0 || reads > 2 || calls > 3 {
				t.Fatal("repair exceeded evidence capability/budget", external, reads, calls)
			}
			if mode == "truncated" {
				if err == nil || got != "" {
					t.Fatal("truncated patch accepted", got, err)
				}
			} else if err != nil || got != "microATX supported" {
				t.Fatal(got, err)
			}
			if mode == "normal" && reads != 1 {
				t.Fatal("saved reader not called", reads)
			}
			if mode == "limit" && (reads != 2 || calls != 3) {
				t.Fatal("tool rounds not bounded", reads, calls)
			}
		})
	}
}
