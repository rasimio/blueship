package gateway

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

func (g *Gateway) authorizeExecution(
	ctx context.Context,
	userID, soulID uuid.UUID,
	kind bs.ExecutionKind,
	transport string,
) (bs.ExecutionDecision, error) {
	if g.deps.AuthorizeExecution == nil {
		return bs.ExecutionDecision{Allowed: true}, nil
	}
	decision, err := g.deps.AuthorizeExecution(ctx, bs.ExecutionRequest{
		UserID: userID, SoulID: soulID, Kind: kind, Transport: transport,
	})
	if err != nil {
		return bs.ExecutionDecision{}, fmt.Errorf("authorize execution: %w", err)
	}
	return decision, nil
}
