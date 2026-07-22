package gateway

import (
	"context"
	"testing"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestAuthorizeExecutionDefaultsToAllow(t *testing.T) {
	g := &Gateway{deps: &bs.Deps{}}
	decision, err := g.authorizeExecution(
		context.Background(), uuid.New(), uuid.New(), bs.ExecutionInteractive, "test",
	)
	if err != nil || !decision.Allowed {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestAuthorizeExecutionPassesGenericContext(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	var got bs.ExecutionRequest
	g := &Gateway{deps: &bs.Deps{
		AuthorizeExecution: func(_ context.Context, request bs.ExecutionRequest) (bs.ExecutionDecision, error) {
			got = request
			return bs.ExecutionDecision{Allowed: false, Reason: "host_policy"}, nil
		},
	}}
	decision, err := g.authorizeExecution(
		context.Background(), userID, soulID, bs.ExecutionInteractive, "telegram",
	)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Allowed || decision.Reason != "host_policy" {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	if got.UserID != userID || got.SoulID != soulID || got.Kind != bs.ExecutionInteractive || got.Transport != "telegram" {
		t.Fatalf("unexpected request: %+v", got)
	}
}
