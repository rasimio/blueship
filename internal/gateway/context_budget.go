package gateway

import "github.com/rasimio/blueship/internal/core"

func (g *Gateway) messageBudgetForRole(role string, ref core.ModelRef) core.MessageBudgetDecision {
	if ref.MessageBudget <= 0 && g.deps.ModelStore != nil {
		if dbRef := g.deps.ModelStore.Get(role); dbRef.MessageBudget > 0 {
			ref = dbRef
		}
	}
	return core.ResolveMessageBudget(core.MessageBudgetRequest{
		Role:     role,
		ModelRef: ref,
		Config:   g.deps.Config,
	})
}
