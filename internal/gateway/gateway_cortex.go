package gateway

import (
	"context"
	"encoding/base64"
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

// videoSystemPrompt drives the same reader over a strip of stills cut from one
// recording rather than over separate pictures. It asks for exactly what a
// transcript cannot carry: where the person is, what they are doing, what they
// hold up to the camera, and what changes between the frames.
const videoSystemPrompt = `You watch videos for another assistant that cannot see them. It will answer the user using only your description, so describe rather than answer.

The images are frames sampled in order from a single recording, each preceded by its timestamp. Read them as one continuous scene, not as unrelated pictures. Say what is going on: who is there, where they are, what they are doing or showing to the camera, any text or objects they hold up, and what changes from frame to frame. If the frames are nearly identical — someone talking to the camera without moving — say that in one line instead of describing each frame in turn.

You may also be given the speech from the recording. Use it to make sense of what you see, and point out where the two meet: someone showing the thing they are talking about. Do not repeat the speech back — the assistant already has it.

Do not address the user, do not offer advice, opinions or next steps, and do not guess at anything the frames do not show. If something is unreadable or ambiguous, say so plainly instead of inventing it.

Write in the language given by [user language], unless the user's own message is written in another language — then match their message. Fall back to English only when neither is given. Never infer the language from the speech: a recording whose whole soundtrack is one word, a product name or somebody's name, says nothing about what language the conversation is in, and a description that switches language reads as machine noise to whoever gets it next.`

// videoBlockOpen/Close bracket a whole recording — speech and picture
// together. The header names both halves because the two arriving as separate
// tagged blocks read as an utterance plus some metadata, and the metadata is
// the part that gets skipped.
const (
	videoBlockOpen  = "[video — one recording the user sent: `said` is their speech, `seen` is what the camera showed]"
	videoBlockClose = "\n[/video]"
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

	description, err := g.readVisual(ctx, blocks, visionSystemPrompt, ref, model)
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

// readVisual asks the vision model to read every image in one call, so it can
// describe them in relation to each other rather than one blind caption at a
// time. The system prompt is the caller's: the same model reads a photo album
// and a strip of video frames, but it is told a different thing about them.
func (g *Gateway) readVisual(ctx context.Context, blocks []bs.ContentBlock, system string, ref bs.ModelRef, model string) (string, error) {
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
		System:        system,
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

// describeVideoFrames reads a sampled recording into text, using the same
// model row as images: one reader, one place to retune it. Returns an empty
// description when no vision row is configured, which leaves the turn with the
// transcript alone — the behaviour before video had a picture at all.
//
// The description is query-conditioned like the image one: the user's own
// words go to the reader, so "что у меня на фоне" gets answered from the
// background rather than from whoever is talking in the foreground.
func (g *Gateway) describeVideoFrames(ctx context.Context, frames []videoFrame, transcript, userText, language string) (string, error) {
	if len(frames) == 0 || !g.canReadVisual() {
		return "", nil
	}
	ref := g.deps.ModelStore.Get(visionRole)
	model := g.deps.ModelStore.ForRouter(visionRole)
	return g.readVisual(ctx, videoFramePrompt(frames, transcript, userText, language), videoSystemPrompt, ref, model)
}

// canReadVisual reports whether a model row is configured to read pixels on
// behalf of a cortex that cannot. Without one, images pass through untouched
// and frames are not sampled at all.
func (g *Gateway) canReadVisual() bool {
	if g.deps == nil || g.deps.ModelStore == nil || g.provider == nil {
		return false
	}
	return g.deps.ModelStore.Get(visionRole).Name != "" && g.deps.ModelStore.ForRouter(visionRole) != ""
}

// videoFramePrompt lays the recording out for the reader: the user's question
// first, then the speech, then the stills each behind their own timestamp.
// Interleaving the timestamps as text blocks is what lets the reader talk
// about when something happened instead of counting images.
func videoFramePrompt(frames []videoFrame, transcript, userText, language string) []bs.ContentBlock {
	blocks := make([]bs.ContentBlock, 0, len(frames)*2+4)
	if tag := strings.TrimSpace(language); tag != "" {
		blocks = append(blocks, bs.ContentBlock{Type: "text", Text: "[user language] " + tag})
	}
	if text := strings.TrimSpace(userText); text != "" {
		blocks = append(blocks, bs.ContentBlock{Type: "text", Text: "[user message]\n" + text})
	}
	if speech := strings.TrimSpace(transcript); speech != "" {
		blocks = append(blocks, bs.ContentBlock{Type: "text", Text: "[speech in the recording]\n" + speech})
	}
	blocks = append(blocks, bs.ContentBlock{
		Type: "text",
		Text: fmt.Sprintf("[%d frames from one recording follow, in order]", len(frames)),
	})
	for _, frame := range frames {
		blocks = append(blocks,
			bs.ContentBlock{Type: "text", Text: formatFrameOffset(frame.offset)},
			bs.ContentBlock{Type: "image", Source: &bs.ImageSource{
				Type:      "base64",
				MediaType: "image/jpeg",
				Data:      base64.StdEncoding.EncodeToString(frame.jpeg),
			}},
		)
	}
	return blocks
}

func formatFrameOffset(offset time.Duration) string {
	seconds := int(offset.Round(time.Second).Seconds())
	return fmt.Sprintf("[%02d:%02d]", seconds/60, seconds%60)
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

	// The dialogue window now starts at the last soft summary, so this text is
	// the only thing carrying what happened before that point. Loading it is not
	// an optimisation: without it, anchoring the window would hand the model LESS
	// history than it had before, which is the opposite of the intent.
	//
	// A failure is logged and survived. Losing the summary costs older context;
	// refusing the turn costs the conversation.
	// A disabled threshold switches session summaries off COMPLETELY — the
	// writer (gated in maybeScheduleSoftSummary) and this reader together.
	// Gating only the writer would let the last stored summary outlive the
	// decision to stop trusting summaries, riding every prompt indefinitely:
	// production's freshest "summary" was a verbatim echo of one old reply
	// about a router hookup, presented as the memory of 569 messages.
	//
	// The dialogue window pairs with this text downstream: the loop anchors
	// at the summary boundary only when compactSummary is non-empty, so an
	// empty summary here also returns the window to a plain budget tail.
	compactSummary := ""
	if summariesEnabled(g.deps.Config.Limits.SoftSummaryThreshold) {
		compactSummary = derefString(sess.CompactSummary)
		if g.store == nil {
			// No session persistence configured — there is no summary to read
			// and nothing has been anchored, so the window is a plain tail.
			// Same guard the other store readers here carry.
		} else if soft, softErr := g.store.LatestSoftSummaryText(ctx, sess.ID); softErr != nil {
			g.logger.Error("soft summary: load for prompt failed, older context is missing from this turn",
				"session_id", sess.ID, "error", softErr)
		} else {
			compactSummary = composeSummaries(compactSummary, soft)
		}
	}

	return preparedCortexTurn{
		loop:     loop,
		registry: turnRegistry,
		now:      now,
		config: agent.RunConfig{
			SessionID: sess.ID,
			// No clock here. The stamp changes every minute and this is the head
			// of the cacheable prefix, so it used to invalidate the entire
			// prompt on every message; the loop now carries it in the turn
			// context, which is rebuilt each message anyway. TurnNow below is
			// what it renders from.
			SystemPrompt:        soulPrompt,
			CompactSummary:      compactSummary,
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

// composeSummaries joins the hard-compaction summary with the soft one, oldest
// first, dropping whichever is absent.
//
// Order is the whole content of this function. Hard compaction summarises and
// deletes; a soft summary is appended later and covers newer ground. Reversed,
// the model reads the recent past as if it came before the distant past, which is
// worse than having neither — a wrong order is asserted with the same confidence
// as a right one.
func composeSummaries(hard, soft string) string {
	parts := make([]string, 0, 2)
	for _, s := range []string{hard, soft} {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, "\n\n")
}
