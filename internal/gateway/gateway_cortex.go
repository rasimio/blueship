package gateway

import (
	"context"
	"fmt"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
	"github.com/rasimio/blueship/runtime/session"
)

type preparedCortexTurn struct {
	loop     *agent.Loop
	registry *bs.ToolRegistry
	config   agent.RunConfig
	now      time.Time
}

// prepareCortexTurn is the common identity/model/history-window builder used
// by both inbound and assistant-initiated chat turns. Context preparation is
// intentionally outside: interactive and autonomous origins have different
// rule/tool admission, but reach the same soul, Cortex model, and budgets.
func (g *Gateway) prepareCortexTurn(
	ctx context.Context,
	us *UserState,
	sess *session.Session,
	injectedCtx, reflexGuidance string,
	timings *turnTimer,
	noTools bool,
) (preparedCortexTurn, error) {
	turnRegistry := us.Registry
	if noTools {
		turnRegistry = bs.NewToolRegistry()
	} else if g.deps.Config.MCPSource != nil {
		if mcpTools := g.deps.Config.MCPSource.ToolsForSoul(ctx, us.SoulID); len(mcpTools) > 0 {
			turnRegistry = us.Registry.Clone()
			for _, t := range mcpTools {
				turnRegistry.RegisterRemote(t.Name, t.Description, t.Schema, bs.ToolModeSync, "mcp", t.Handler)
			}
		}
	}

	loop := agent.NewLoop(g.provider, g.store, turnRegistry, g.deps.RoleTools, g.deps.Config, g.logger)
	now := time.Now().In(g.deps.Config.Gateway.TimezoneFor(bs.WithSoulID(ctx, us.SoulID), g.tz))
	promptStarted := time.Now()
	soulPrompt, err := g.systemPromptForSoul(ctx, us.SoulID)
	if timings != nil {
		timings.RecordSince("gateway.system_prompt", promptStarted, "")
	}
	if err != nil {
		return preparedCortexTurn{}, err
	}

	var cortexRef bs.ModelRef
	if g.deps.ModelStore != nil {
		cortexRef = g.deps.ModelStore.Get("cortex")
	}
	cortexMaxTokens := g.deps.Config.Limits.MaxOutputTokens
	if cortexRef.MaxTokens > 0 {
		cortexMaxTokens = cortexRef.MaxTokens
	}
	turnMessageBudget := g.messageBudgetForRole("cortex", cortexRef)
	var onTiming func(bs.TimingSpan)
	if timings != nil {
		onTiming = timings.Add
	}
	var allowedTools []string
	if noTools {
		allowedTools = []string{}
	} else {
		allowedTools = g.allowedToolsForSoul(ctx, us.SoulID, turnRegistry)
	}

	return preparedCortexTurn{
		loop:     loop,
		registry: turnRegistry,
		now:      now,
		config: agent.RunConfig{
			SessionID:           sess.ID,
			SystemPrompt:        fmt.Sprintf("[current_datetime: %s]\n\n%s", now.Format("2006-01-02 15:04 MST (Monday)"), soulPrompt),
			CompactSummary:      derefString(sess.CompactSummary),
			Model:               g.cortexModel(),
			MaxTokens:           cortexMaxTokens,
			ContextWindow:       cortexRef.ContextWindow,
			MaxTurns:            g.deps.Config.Gateway.MaxTurns,
			InjectedContext:     injectedCtx,
			ReflexGuidance:      reflexGuidance,
			Role:                "cortex",
			Temperature:         cortexRef.Temperature,
			MessageBudget:       turnMessageBudget.Budget,
			MessageBudgetSource: turnMessageBudget.Source,
			ThinkingBudget:      bs.ThinkingBudgetForModelRef(cortexRef),
			ThinkingMode:        cortexRef.ThinkingMode,
			TurnNow:             now,
			Effort:              cortexRef.Effort,
			AllowedTools:        allowedTools,
			OnTiming:            onTiming,
		},
	}, nil
}
