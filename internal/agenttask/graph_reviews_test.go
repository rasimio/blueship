package agenttask

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/rasimio/blueship/internal/core"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type parallelGateProvider struct {
	started    atomic.Int32
	both       chan struct{}
	auditEnded chan struct{}
	fail       bool
}

func TestParseAcceptanceRequiresCompleteExplicitVerdict(t *testing.T) {
	for _, tc := range []struct {
		body, stop string
		valid, met bool
	}{
		{`{"met":false,"reason":"Over budget"}`, "end_turn", true, false},
		{`{"met":true}`, "end_turn", true, true},
		{`{"reason":"Over budget"}`, "end_turn", false, false},
		{`{"met":"false"}`, "end_turn", false, false},
		{`{"met":false}`, "max_tokens", false, false},
		{`broken`, "end_turn", false, false},
	} {
		verdict, valid := parseAcceptanceResponse(&core.CompletionResponse{StopReason: tc.stop, Content: []core.ContentBlock{{Type: "text", Text: tc.body}}})
		if valid != tc.valid || (valid && verdict.Met != tc.met) {
			t.Fatal(tc, verdict, valid)
		}
	}
	if _, valid := parseAcceptanceResponse(nil); valid {
		t.Fatal("nil response accepted")
	}
}

func (p *parallelGateProvider) Complete(ctx context.Context, req core.CompletionRequest) (*core.CompletionResponse, error) {
	if p.started.Add(1) == 2 {
		close(p.both)
	}
	select {
	case <-p.both:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if strings.HasPrefix(req.System, groundingSystemPrompt) {
		defer close(p.auditEnded)
		if p.fail {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"claims":[{"claim":"unsupported number","claim_type":"numerical","status":"ungrounded","supporting_doc_url":"","supporting_span":"","issue":"not in source"}]}`}}}, nil
	}
	if p.fail {
		return nil, core.NewHTTPFailure(429, "90", errors.New("rate limited"))
	}
	return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"met":true,"reason":"structure complete"}`}}}, nil
}
func TestGraphReviewsRunConcurrentlyAndDrainAudit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		p := &parallelGateProvider{both: make(chan struct{}), auditEnded: make(chan struct{}), fail: fail}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		deps := evaluatorTestDeps(p, nil)
		task := criteriaTask("Check facts")
		task.ExecutorVersion = 2
		response, grounding, _, err := completeGraphReviews(ctx, deps, task, "unsupported number", []ToolOutput{{Output: "source has no number"}}, core.CompletionRequest{System: "acceptance"}, nil)
		cancel()
		if p.started.Load() != 2 || grounding == nil {
			t.Fatal("gates were not concurrent", p.started.Load(), grounding, err)
		}
		select {
		case <-p.auditEnded:
		default:
			t.Fatal("audit left running")
		}
		if fail {
			retry, delay := core.TaskRetryPolicy(err)
			if !retry || delay != 90*time.Second || response != nil || !grounding.Unavailable {
				t.Fatal(response, grounding, err)
			}
		} else if err != nil || response == nil || grounding.Unavailable || grounding.Met || grounding.UngroundedCount != 1 {
			t.Fatal("independent audit result lost", response, grounding, err)
		}
	}
}

type cachedGateProvider struct {
	audits  atomic.Int32
	accepts atomic.Int32
}

func (p *cachedGateProvider) Complete(_ context.Context, req core.CompletionRequest) (*core.CompletionResponse, error) {
	text := `{"met":true,"reason":"complete"}`
	if strings.HasPrefix(req.System, groundingSystemPrompt) {
		p.audits.Add(1)
		text = `{"claims":[{"claim":"height 159 mm","claim_type":"numerical","status":"grounded","supporting_doc_url":"https://example.com/spec","supporting_span":"height 159 mm"}]}`
	} else {
		p.accepts.Add(1)
	}
	return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: text}}}, nil
}
func TestGraphReviewsReuseOnlyExactCompletedAudit(t *testing.T) {
	for _, mode := range []string{"same", "report_changed", "source_changed", "policy_changed", "unavailable", "negative"} {
		t.Run(mode, func(t *testing.T) {
			provider := &cachedGateProvider{}
			deps := evaluatorTestDeps(provider, nil)
			task := criteriaTask("Check height")
			task.ExecutorVersion = 2
			docs := []ToolOutput{{Output: "height 159 mm"}}
			report := "height 159 mm"
			request := core.CompletionRequest{System: "acceptance"}
			_, grounding, hash, err := completeGraphReviews(context.Background(), deps, task, report, docs, request, nil)
			if err != nil || grounding == nil || grounding.Unavailable || hash == "" {
				t.Fatal(grounding, hash, err)
			}
			prior := AcceptanceVerdict{Unavailable: true, Grounding: grounding, GroundingInputHash: hash}
			raw, err := json.Marshal(prior)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &prior); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "report_changed":
				report = "height 160 mm"
			case "source_changed":
				docs[0].Output = "height 160 mm"
			case "policy_changed":
				deps.Config.Models.Primary.Effort = "low"
			case "unavailable":
				prior.Grounding.Unavailable = true
			case "negative":
				prior.Grounding.Met = false
				prior.Grounding.Reason = "source veto"
			}
			_, result, _, err := completeGraphReviews(context.Background(), deps, task, report, docs, request, &prior)
			wantAudits := int32(2)
			if mode == "same" || mode == "negative" {
				wantAudits = 1
			}
			if err != nil || provider.audits.Load() != wantAudits || provider.accepts.Load() != 2 {
				t.Fatal(mode, provider.audits.Load(), provider.accepts.Load(), err)
			}
			if mode == "negative" && (result.Met || result.Reason != "source veto") {
				t.Fatal("cached veto lost", result)
			}
		})
	}
}
