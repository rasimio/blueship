package handler

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

// Retain the final response separately from the loop's accumulated prose:
// intermediate tool narration is not part of the exact JSON edit document.
type graphRepairProvider struct {
	core.CompletionProvider
	last *core.CompletionResponse
}

func (p *graphRepairProvider) Complete(ctx context.Context, request core.CompletionRequest) (*core.CompletionResponse, error) {
	response, err := p.CompletionProvider.Complete(ctx, request)
	p.last = response
	return response, err
}

// Use the same bounded agent loop, but replace the registry with handlers that
// can only read this task's persisted evidence. No ordinary handler survives.
func graphRepairEvidence(ctx context.Context, task core.AgentTask, deps core.AgentDeps, model core.ModelRef, prompt, input string) (*core.CompletionResponse, bool, error) {
	if task.ExecutorVersion != 2 || deps.Registry == nil {
		return nil, false, nil
	}
	registry := deps.Registry.EvidenceOnlySubset()
	definitions := registry.Definitions()
	if len(definitions) == 0 {
		return nil, false, nil
	}
	if deps.Store == nil || task.ID == uuid.Nil {
		return nil, true, fmt.Errorf("saved-evidence repair requires task and session store")
	}
	ctx = core.ContextWithIteration(core.ContextWithTaskID(core.WithSoulID(ctx, task.SoulID), task.ID), max(1, task.Iteration))
	session, err := deps.Store.CreateSessionWithSource(ctx, task.UserID.String(), model.Name, "agent_task", task.ID.String())
	if err != nil {
		return nil, true, err
	}
	allowed := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		allowed = append(allowed, definition.Name)
	}
	provider := &graphRepairProvider{CompletionProvider: deps.LLM}
	loop := agent.NewLoop(provider, deps.Store, registry, deps.RoleTools, deps.Config, deps.Logger)
	result, err := loop.RunTracked(ctx, agent.RunConfig{SessionID: session, SystemPrompt: prompt, Model: model.ForRouter(), Role: modelRoleForTask(task), MaxTokens: 16384, MaxTurns: 3, MaxToolTurns: 2, ToolOverride: allowed, StrictTools: true, ParallelReadTools: core.BackgroundParallelReads, Effort: model.Effort, ThinkingMode: model.ThinkingMode}, input)
	if err != nil {
		return nil, true, err
	}
	if result == nil || (result.Outcome.Reason != agent.RunStopCompleted && result.Outcome.Reason != agent.RunStopToolBudget) {
		return nil, true, fmt.Errorf("saved-evidence repair did not produce a complete response")
	}
	return provider.last, true, nil
}
