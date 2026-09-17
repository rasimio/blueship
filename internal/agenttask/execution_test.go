package agenttask

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/internal/core"
)

func TestExecutionAllowedUsesBackgroundAdmission(t *testing.T) {
	userID, soulID, taskID := uuid.New(), uuid.New(), uuid.New()
	var got core.ExecutionRequest
	s := &Scheduler{
		deps: &core.Deps{AuthorizeExecution: func(_ context.Context, request core.ExecutionRequest) (core.ExecutionDecision, error) {
			got = request
			return core.ExecutionDecision{Allowed: false, Reason: "host_policy"}, nil
		}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if s.executionAllowed(context.Background(), core.AgentTask{ID: taskID, UserID: userID, SoulID: soulID}) {
		t.Fatal("denied task was admitted")
	}
	if got.UserID != userID || got.SoulID != soulID || got.Kind != core.ExecutionBackground || got.Transport != "agent_task" {
		t.Fatalf("unexpected request: %+v", got)
	}
	// The host tells its tasks apart by the task itself; without it every
	// background tick is indistinguishable at the admission seam.
	if got.Task == nil || got.Task.ID != taskID {
		t.Fatalf("request does not carry the task: %+v", got.Task)
	}
}
