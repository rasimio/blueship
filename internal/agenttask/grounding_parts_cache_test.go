package agenttask

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rasimio/blueship/internal/core"
)

type resumeAuditProvider struct {
	calls    atomic.Int32
	truncate atomic.Bool
	negative bool
}

func (p *resumeAuditProvider) Complete(_ context.Context, req core.CompletionRequest) (*core.CompletionResponse, error) {
	body := `{"met":true,"reason":"complete"}`
	if strings.HasPrefix(req.System, groundingSystemPrompt) {
		p.calls.Add(1)
		user := core.ExtractText(core.NormalizeContent(req.Messages[len(req.Messages)-1].Content))
		_, target, _ := strings.Cut(user, "\n\n[audit_target]\n")
		if strings.HasPrefix(target, "B") && p.truncate.Load() {
			return &core.CompletionResponse{StopReason: "max_tokens"}, nil
		}
		status := "grounded"
		if p.negative && strings.HasPrefix(target, "A") {
			status = "partial"
		}
		raw, _ := json.Marshal(map[string]any{"claims": []ClaimGrounding{{Claim: target[:1], ClaimType: "numerical", Status: status}}})
		body = string(raw)
	}
	return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: body}}}, nil
}

func TestGroundingResumesOnlyExactPersistedPartitions(t *testing.T) {
	for _, mode := range []string{"same", "negative", "report", "source", "observed_at", "model", "corrupt_part"} {
		t.Run(mode, func(t *testing.T) {
			p := &resumeAuditProvider{negative: mode == "negative"}
			p.truncate.Store(true)
			deps := evaluatorTestDeps(p, nil)
			task := core.AgentTask{ExecutorVersion: 2}
			report := "A" + strings.Repeat("a", 2000) + "\nB" + strings.Repeat("b", 2000) + "\nC" + strings.Repeat("c", 2000)
			docs := []ToolOutput{{ToolName: "browser_fetch", Output: "evidence", Metadata: map[string]any{"observed_at": "2026-09-22T19:00:00Z"}}}
			request := core.CompletionRequest{System: "acceptance"}
			_, first, hash, err := completeGraphReviews(context.Background(), deps, task, report, docs, request, nil)
			if err != nil || first == nil || !first.Unavailable || len(first.AuditedParts) != 2 || p.calls.Load() != 3 {
				t.Fatal("first audit not checkpointed", first, err, p.calls.Load())
			}
			// The same serialization used by persisted scheduler verification state.
			raw, _ := json.Marshal(AcceptanceVerdict{Unavailable: true, Grounding: first, GroundingInputHash: hash})
			var prior AcceptanceVerdict
			if err = json.Unmarshal(raw, &prior); err != nil {
				t.Fatal(err)
			}
			expectedCalls := int32(4)
			switch mode {
			case "report":
				report += " changed"
				expectedCalls = 6
			case "source":
				docs[0].Output += " changed"
				expectedCalls = 6
			case "observed_at":
				docs[0].Metadata["observed_at"] = "2026-09-23T19:00:00Z"
				expectedCalls = 6
			case "model":
				deps.Config.Models.Primary.Effort = "low"
				expectedCalls = 6
			case "corrupt_part":
				prior.Grounding.AuditedParts[0].Claims = nil
				expectedCalls = 5
			}
			p.truncate.Store(false)
			_, second, _, err := completeGraphReviews(context.Background(), deps, task, report, docs, request, &prior)
			if err != nil || second == nil || second.Unavailable || second.Met == (mode == "negative") || len(second.AuditedParts) != 3 || p.calls.Load() != expectedCalls {
				t.Fatal("incorrect resumed verdict/call count", second, err, p.calls.Load(), expectedCalls)
			}
		})
	}
}
