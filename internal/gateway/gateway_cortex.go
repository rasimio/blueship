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

func (g *Gateway) cortexTurnRegistry(ctx context.Context, us *UserState, noTools bool) *bs.ToolRegistry {
	if noTools || us == nil || us.Registry == nil {
		return bs.NewToolRegistry()
	}
	turnRegistry := us.Registry
	if g.deps.Config.MCPSource == nil {
		return turnRegistry
	}
	if mcpTools := g.deps.Config.MCPSource.ToolsForSoul(ctx, us.SoulID); len(mcpTools) > 0 {
		turnRegistry = us.Registry.Clone()
		for _, t := range mcpTools {
			turnRegistry.RegisterRemote(t.Name, t.Description, t.Schema, bs.ToolModeSync, "mcp", t.Handler)
		}
	}
	return turnRegistry
}

// prepareCortexTurn is the common identity/model/history-window builder used
// by both inbound and assistant-initiated chat turns. Context preparation is
// intentionally outside: interactive and autonomous origins have different
// rule/tool admission, but reach the same soul, Cortex model, and budgets.
// visionRole is the optional model_config role a turn is routed to when it
// carries images. Leaving the row out keeps every deployment on its normal
// cortex model — the override exists, it just never fires.
const visionRole = "vision"

// applyVisionModel re-points a turn at the vision role when it carries image
// content. A text-only cortex cannot answer such a turn at all: depending on
// the provider it either rejects the request outright or has the image stripped
// and answers blind.
//
// Every generation control is taken from the vision row, never inherited from
// cortex. Reasoning settings are not portable between models — a tier that
// inherited another tier's effort and thinking mode once broke chat with a 400.
// Reported as a bool so the caller can log the swap rather than have models
// change silently underfoot.
func (g *Gateway) applyVisionModel(cfg *agent.RunConfig, content any) bool {
	if g.deps.ModelStore == nil || cfg == nil || !hasImageContent(content) {
		return false
	}
	ref := g.deps.ModelStore.Get(visionRole)
	model := g.deps.ModelStore.ForRouter(visionRole)
	if ref.Name == "" || model == "" {
		return false
	}

	cfg.Model = model
	if ref.MaxTokens > 0 {
		cfg.MaxTokens = ref.MaxTokens
	}
	cfg.ContextWindow = ref.ContextWindow
	cfg.Temperature = ref.Temperature
	cfg.ThinkingBudget = bs.ThinkingBudgetForModelRef(ref)
	cfg.ThinkingMode = ref.ThinkingMode
	cfg.Effort = ref.Effort

	budget := g.messageBudgetForRole(visionRole, ref)
	cfg.MessageBudget = budget.Budget
	cfg.MessageBudgetSource = budget.Source
	return true
}

func (g *Gateway) prepareCortexTurn(
	ctx context.Context,
	us *UserState,
	sess *session.Session,
	injectedCtx, reflexGuidance string,
	timings *turnTimer,
	noTools bool,
) (preparedCortexTurn, error) {
	return g.prepareCortexTurnWithRegistry(
		ctx, us, sess, injectedCtx, reflexGuidance, timings, noTools, nil, nil,
	)
}

func (g *Gateway) prepareCortexTurnWithRegistry(
	ctx context.Context,
	us *UserState,
	sess *session.Session,
	injectedCtx, reflexGuidance string,
	timings *turnTimer,
	noTools bool,
	turnRegistry *bs.ToolRegistry,
	allowedToolsSnapshot *[]string,
) (preparedCortexTurn, error) {
	if noTools {
		turnRegistry = bs.NewToolRegistry()
	} else if turnRegistry == nil {
		turnRegistry = g.cortexTurnRegistry(ctx, us, noTools)
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
	} else if allowedToolsSnapshot != nil {
		allowedTools = cloneStrings(*allowedToolsSnapshot)
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
