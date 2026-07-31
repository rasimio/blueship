package gateway

import (
	"context"
	"fmt"
	"strings"
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
// visionRole is the optional model_config role that reads images on behalf of a
// cortex model that cannot see them. Leave the row out and images are passed
// through untouched, for a cortex that reads them natively.
const visionRole = "vision"

// visionSystemPrompt drives the reader. It must stay a describer: the answer
// the user reads is always written by cortex, with its own persona, memory and
// tools. A reader that starts answering would speak in a voice the user never
// chose, and would do it without any of that context.
const visionSystemPrompt = `You read images for another assistant that cannot see them. It will answer the user using only your description, so describe rather than answer.

Lead with whatever the user's message actually asks about — that is the part that matters most. Then give the general content: layout, any visible text transcribed verbatim, people, objects, colours, numbers, and anything else that would be needed to discuss the image.

Do not address the user, do not offer advice, opinions or next steps, and do not guess at anything the image does not show. If something is unreadable or ambiguous, say so plainly instead of inventing it. Write in the language of the user's message.`

// visionDescriptionOpen/Close bracket the description in the same tag style the
// rest of the prompt uses, so cortex can tell a machine-made reading of an
// image from the user's own words.
const (
	visionDescriptionOpen  = "[image_description]\n"
	visionDescriptionClose = "\n[/image_description]"
)

// describeImages replaces image blocks with a textual reading of them produced
// by the vision model, leaving the answer itself to cortex. Returns the content
// to send and whether anything was replaced.
//
// The description is query-conditioned: the user's own message goes to the
// reader, so it extracts what is actually being asked about instead of a
// generic caption that misses the small print in the corner.
//
// Without a vision row the content is returned untouched, which is the correct
// behaviour for a cortex that reads images natively. Any failure also falls
// through to the original content: a turn that reaches a blind model is a worse
// answer, while a dropped turn is no answer at all.
func (g *Gateway) describeImages(ctx context.Context, content any) (any, bool) {
	if g.deps.ModelStore == nil || g.provider == nil || !hasImageContent(content) {
		return content, false
	}
	blocks, ok := content.([]bs.ContentBlock)
	if !ok {
		return content, false
	}
	ref := g.deps.ModelStore.Get(visionRole)
	model := g.deps.ModelStore.ForRouter(visionRole)
	if ref.Name == "" || model == "" {
		return content, false
	}

	description, err := g.readImages(ctx, blocks, ref, model)
	if err != nil {
		g.logger.Error("vision: could not read images, passing them through",
			"model", model, "error", err)
		return content, false
	}
	if strings.TrimSpace(description) == "" {
		g.logger.Warn("vision: reader returned nothing, passing images through", "model", model)
		return content, false
	}
	return replaceImagesWithDescription(blocks, description), true
}

// readImages asks the vision model to read every image in the turn in one call,
// so it can describe them in relation to each other rather than one blind
// caption at a time.
func (g *Gateway) readImages(ctx context.Context, blocks []bs.ContentBlock, ref bs.ModelRef, model string) (string, error) {
	prompt := make([]bs.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "image" || b.Type == "text" {
			prompt = append(prompt, b)
		}
	}

	maxTokens := ref.MaxTokens
	if maxTokens <= 0 {
		maxTokens = g.deps.Config.Limits.MaxOutputTokens
	}
	resp, err := g.provider.Complete(ctx, bs.CompletionRequest{
		Model:         model,
		MaxTokens:     maxTokens,
		ContextWindow: ref.ContextWindow,
		System:        visionSystemPrompt,
		Messages:      []bs.Message{{Role: "user", Content: prompt}},
		// Reasoning controls come from the vision row alone; a tier must never
		// inherit another tier's, which has broken chat before with a 400.
		Temperature:    ref.Temperature,
		ThinkingBudget: bs.ThinkingBudgetForModelRef(ref),
		ThinkingMode:   ref.ThinkingMode,
		Effort:         ref.Effort,
	})
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, b := range resp.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	return text.String(), nil
}

// replaceImagesWithDescription swaps every image block for a single description
// block, placed where the first image was so it keeps its position relative to
// the user's own words. Non-image blocks are preserved in order.
func replaceImagesWithDescription(blocks []bs.ContentBlock, description string) []bs.ContentBlock {
	out := make([]bs.ContentBlock, 0, len(blocks))
	placed := false
	for _, b := range blocks {
		if b.Type != "image" {
			out = append(out, b)
			continue
		}
		if placed {
			continue
		}
		out = append(out, bs.ContentBlock{
			Type: "text",
			Text: visionDescriptionOpen + strings.TrimSpace(description) + visionDescriptionClose,
		})
		placed = true
	}
	return out
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
