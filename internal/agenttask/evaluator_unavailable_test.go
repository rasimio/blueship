package agenttask

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

type failedReviewer struct{}

type limitedReviewer struct{}

func (limitedReviewer) Complete(context.Context, core.CompletionRequest) (*core.CompletionResponse, error) {
	return nil, core.NewHTTPFailure(429, "90", errors.New("rate limited"))
}

func TestVerificationPreservesRetryAfter(t *testing.T) {
	deps := evaluatorTestDeps(limitedReviewer{}, nil)
	verdict := evaluateAcceptance(context.Background(), deps, criteriaTask("Report"), "Report", nil)
	if !verdict.Unavailable || verdict.RetryDelay != 90*time.Second {
		t.Fatal(verdict)
	}
	grounding := evaluateGrounding(context.Background(), deps, criteriaTask("Report"), "Report", []ToolOutput{{Output: "source text"}})
	if !grounding.Unavailable || grounding.RetryDelay != 90*time.Second {
		t.Fatal(grounding)
	}
	verdict = unavailableAcceptance(grounding.Reason, &grounding)
	if verdict.RetryDelay != 90*time.Second {
		t.Fatal("grounding delay lost", verdict)
	}
}

func (failedReviewer) Complete(context.Context, core.CompletionRequest) (*core.CompletionResponse, error) {
	return nil, errors.New("provider unavailable")
}

func TestAcceptanceNeverPassesOnReviewerFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider core.CompletionProvider
	}{
		{"missing provider", nil},
		{"provider error", failedReviewer{}},
		{"invalid JSON", &reviewerStub{verdict: "not JSON"}},
		{"missing verdict", &reviewerStub{verdict: `{"reason":"looks done"}`}},
		{"null verdict", &reviewerStub{verdict: `{"met":null}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := evaluateAcceptance(context.Background(), evaluatorTestDeps(tc.provider, nil), criteriaTask("Produce the requested report."), "candidate report", nil)
			if v.Met || !v.Unavailable {
				t.Fatalf("unverified report passed: %+v", v)
			}
		})
	}
}

func TestGroundingNeverPassesOnReviewerFailure(t *testing.T) {
	for _, provider := range []core.CompletionProvider{nil, failedReviewer{}, &reviewerStub{verdict: "not JSON"}} {
		v := evaluateGrounding(context.Background(), evaluatorTestDeps(provider, nil), criteriaTask("Check evidence."), "candidate report", []ToolOutput{{Output: "source content"}})
		if v.Met || !v.Unavailable {
			t.Fatalf("unverified grounding passed: %+v", v)
		}
	}
}

type truncatedAcceptanceReviewer struct {
	t           *testing.T
	nilResponse bool
}

func (r truncatedAcceptanceReviewer) Complete(_ context.Context, req core.CompletionRequest) (*core.CompletionResponse, error) {
	if req.MaxTokens < 4096 {
		r.t.Fatal("verdict budget excludes reasoning", req.MaxTokens)
	}
	if r.nilResponse {
		return nil, nil
	}
	return &core.CompletionResponse{StopReason: "max_tokens", Content: []core.ContentBlock{{Type: "text", Text: `{"met":true,"reason":"apparently complete"}`}}}, nil
}
func TestAcceptanceRejectsTruncatedEvenParseableVerdict(t *testing.T) {
	for _, nilResponse := range []bool{false, true} {
		v := evaluateAcceptance(context.Background(), evaluatorTestDeps(truncatedAcceptanceReviewer{t: t, nilResponse: nilResponse}, nil), criteriaTask("Produce report"), "Report", nil)
		if v.Met || !v.Unavailable {
			t.Fatal("incomplete verdict accepted", v)
		}
	}
}

type truncatedGroundingReviewer struct{ nilResponse bool }

func (r truncatedGroundingReviewer) Complete(context.Context, core.CompletionRequest) (*core.CompletionResponse, error) {
	if r.nilResponse {
		return nil, nil
	}
	return &core.CompletionResponse{StopReason: "max_tokens", Content: []core.ContentBlock{{Type: "text", Text: `{"claims":[{"claim":"A fact","claim_type":"numerical","status":"grounded","supporting_doc_url":"https://example.com","supporting_span":"A fact"}]}`}}}, nil
}
func TestGroundingRejectsTruncatedEvenParseableAudit(t *testing.T) {
	for _, nilResponse := range []bool{false, true} {
		v := evaluateGrounding(context.Background(), evaluatorTestDeps(truncatedGroundingReviewer{nilResponse: nilResponse}, nil), criteriaTask("Check facts"), "A fact", []ToolOutput{{Output: "A fact"}})
		if v.Met || !v.Unavailable {
			t.Fatal("incomplete audit accepted", v)
		}
	}
}
