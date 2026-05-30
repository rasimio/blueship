package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rasimio/blueship/agent"
	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/tool"
)

type reflexPipelineResult struct {
	InjectedCtx     string
	ReflexGuidance  string
	PostActions     []bs.PostAction
	PreTraces       []agent.ToolTrace
	EngineRuleCount int
	MemoriesCount   int
	MatchedRules    []bs.MatchedRule
	Strategy        string
	Silent          bool
}

// runReflexPipeline executes the System 1/2 pipeline:
// 1. ReflexPreparer → structured context (traces + candidate rules)
// 2. Reflex LLM (Gemini Flash) → plan (matched rules, pre/post actions, tools)
// 3. Execute pre-actions (web_search etc.) → inject results into context
// 4. Build cortex context: matched rules + research + AME traces
//
// When result.Silent=true the caller MUST abort the turn without calling
// cortex or sending any output — a structured rule with Silent=true matched.
func (g *Gateway) runReflexPipeline(ctx context.Context, us *UserState, msgText, priorContext string) reflexPipelineResult {
	// Interaction tier: skip the ReflexPreparer entirely. The full AME pass
	// (memory_associate + scoring + diversity filter + emotion detection)
	// costs ~3-5 s per turn and the streaming reflex doesn't need it — for
	// chatty/social turns it answers from session history alone, and for
	// memory-needing turns it escalates to cortex which still has the full
	// chat history and all tools. We only run the structured rule engine
	// here (cheap; catches Silent rules and injects scope-based guidance).
	if g.deps.Config.Gateway.InteractionTier {
		var guidance strings.Builder
		hasRules := false
		engineRuleCount := 0
		var matchedRules []bs.MatchedRule
		if us.Deps.RuleEngine != nil {
			engineRules := us.Deps.RuleEngine(ctx, bs.RuleContext{
				UserID:  us.Deps.UserID.String(),
				Hour:    time.Now().Hour(),
				Message: msgText,
			})
			for _, r := range engineRules {
				if r.Silent {
					g.logger.Info("rule engine: silent rule matched, aborting turn",
						"rule_id", r.ID, "trigger", r.Trigger, "chat_id", us.ChatID)
					return reflexPipelineResult{Silent: true}
				}
			}
			for _, r := range engineRules {
				if !hasRules {
					guidance.WriteString("[active rules]\n")
					hasRules = true
				}
				fmt.Fprintf(&guidance, "WHEN: %s\nDO: %s\n\n", r.Trigger, r.Action)
				matchedRules = append(matchedRules, bs.MatchedRule{
					ID: r.ID, Trigger: r.Trigger, Action: r.Action, Source: "engine",
				})
			}
			engineRuleCount = len(engineRules)
			if engineRuleCount > 0 {
				g.logger.Info("rule engine matched (interaction tier)", "count", engineRuleCount)
			}
		}
		if hasRules {
			guidance.WriteString("[/active rules]")
		}
		return reflexPipelineResult{
			ReflexGuidance:  guidance.String(),
			EngineRuleCount: engineRuleCount,
			MatchedRules:    matchedRules,
		}
	}

	rc := us.Deps.ReflexPreparer(ctx, us.UserID.String(), msgText, priorContext)
	if rc == nil {
		return reflexPipelineResult{}
	}

	// Store emotional strategy for TTS instruct mapping.
	us.LastStrategy = rc.Strategy

	// Build reflex prompt.
	var rulesBlock strings.Builder
	for _, r := range rc.CandidateRules {
		fmt.Fprintf(&rulesBlock, "[%s] WHEN: %s → DO: %s (sr=%.0f%%)\n",
			r.ID, r.Trigger, r.Action, r.SuccessRate*100)
	}

	// Tool list for the reflex prompt: one tool per line with its full
	// description. Reflex needs descriptions to disambiguate semantically
	// close tools — name-only lists force it to guess, which is where
	// most mis-selection bugs come from. The descriptions are the same
	// DB-driven strings the cortex sees, via the per-user registry built
	// during getOrInitUser.
	toolsList := "none configured"
	if us.Registry != nil && g.deps.RoleTools != nil {
		names := g.deps.RoleTools.Get("cortex")
		if len(names) > 0 {
			// Group tools by source: local vs each peer.
			local := &strings.Builder{}
			peerTools := make(map[string]*strings.Builder)

			for _, def := range us.Registry.DefinitionsForNames(names) {
				peer := us.Registry.PeerForTool(def.Name)
				line := fmt.Sprintf("- %s: %s\n", def.Name, strings.TrimSpace(def.Description))
				if peer == "" {
					local.WriteString(line)
				} else {
					if peerTools[peer] == nil {
						peerTools[peer] = &strings.Builder{}
					}
					peerTools[peer].WriteString(line)
				}
			}

			var sb strings.Builder
			if local.Len() > 0 {
				sb.WriteString("## Мои инструменты\n")
				sb.WriteString(local.String())
			}
			for peer, buf := range peerTools {
				fmt.Fprintf(&sb, "\n## Инструменты агента %s\n", peer)
				sb.WriteString(buf.String())
			}
			if sb.Len() > 0 {
				toolsList = strings.TrimRight(sb.String(), "\n")
			}
		}
	}

	if g.reflexPlanTemplate == "" {
		g.logger.Warn("reflex-plan prompt not in DB, skipping reflex")
		return reflexPipelineResult{
			InjectedCtx:   rc.FullContext,
			MemoriesCount: rc.MemoriesCount,
			Strategy:      rc.Strategy,
		}
	}
	notesBlock := rc.ActiveNotes
	if notesBlock == "" {
		notesBlock = "(нет активных заметок)"
	}
	reflexPrompt := fmt.Sprintf(g.reflexPlanTemplate, rulesBlock.String(), toolsList, notesBlock, msgText)

	reflexResult, err := g.callReflex(ctx, reflexPrompt)
	if err != nil {
		// Reflex LLM unavailable (e.g. provider 429 / network error).
		// Don't bail out — keep going with full AME context and let
		// the rule engine still inject scope=always/keyword/state
		// guidance, otherwise tool-mandating rules silently disappear
		// whenever the upstream is degraded.
		g.logger.Warn("reflex failed, falling back to full context (rule engine still runs)", "error", err)
		reflexResult = &bs.ReflexResult{Confidence: 0}
	}

	g.logger.Info("reflex plan",
		"intent", reflexResult.Intent,
		"confidence", reflexResult.Confidence,
		"matched_rules", reflexResult.MatchedRules,
		"pre_actions", len(reflexResult.PreActions),
		"post_actions", len(reflexResult.PostActions),
		"tools", reflexResult.Tools,
	)

	// Low confidence → use full context but still run the Rule Engine below.
	// Previously this was a hard return that skipped Rule Engine entirely,
	// causing scope:always rules (like "ВЫЗВАТЬ tool call НЕМЕДЛЕННО") to
	// be silently dropped. Now we only skip reflex-specific outputs
	// (matched_rules, pre_actions) but let the rule engine inject guidance.
	lowConfidence := reflexResult.Confidence < reflexConfidenceThreshold
	if lowConfidence {
		g.logger.Info("reflex low confidence, using full context but keeping rule engine",
			"confidence", reflexResult.Confidence,
		)
		reflexResult.MatchedRules = nil
		reflexResult.PreActions = nil
		reflexResult.PostActions = nil
	}
	formattedTraces := rc.FormattedTraces
	if lowConfidence {
		formattedTraces = rc.FullContext
	}

	// Execute pre-actions (web_search etc.) with timeout.
	var researchBlock strings.Builder
	var preTraces []agent.ToolTrace
	preActionsToRun := reflexResult.PreActions
	if len(preActionsToRun) > maxPreActions {
		preActionsToRun = preActionsToRun[:maxPreActions]
	}
	for _, pa := range preActionsToRun {
		paCtx, cancel := context.WithTimeout(ctx, preActionTimeout)
		result, isError := us.Registry.Execute(paCtx, pa.Tool, pa.Input)
		cancel()
		inputStr := string(pa.Input)
		if len(inputStr) > 200 {
			inputStr = inputStr[:200] + "..."
		}
		outputStr := result
		if len(outputStr) > 500 {
			outputStr = outputStr[:500] + "..."
		}
		preTraces = append(preTraces, agent.ToolTrace{Name: pa.Tool, Input: inputStr, Output: outputStr, Error: isError})
		if isError {
			g.logger.Warn("reflex pre-action failed", "tool", pa.Tool, "error", result)
			continue
		}
		g.logger.Info("reflex pre-action done", "tool", pa.Tool, "result_len", len(result))
		if researchBlock.Len() == 0 {
			researchBlock.WriteString("[research]\n")
		}
		fmt.Fprintf(&researchBlock, "[%s result]\n%s\n\n", pa.Tool, truncateStr(result, 2000))
	}

	// Expand matched rules into directive block (dedup by ID).
	var guidance strings.Builder
	var hasRules bool
	seenRuleIDs := make(map[string]bool)
	var matchedRulesInfo []bs.MatchedRule

	// 0. Disambiguation: reflex detected multiple plausible tools.
	if reflexResult.Intent == "clarification_needed" && len(reflexResult.ClarificationOptions) > 0 {
		guidance.WriteString("[DISAMBIGUATION REQUIRED]\n")
		guidance.WriteString("Запрос неоднозначен. Спроси пользователя что он имеет в виду:\n")
		for i, opt := range reflexResult.ClarificationOptions {
			fmt.Fprintf(&guidance, "%d. %s\n", i+1, opt.Label)
		}
		guidance.WriteString("\nНЕ вызывай инструменты. Задай короткий вопрос с вариантами.\n\n")
		// Save options for resolution on the next turn.
		us.PendingDisambiguation = reflexResult.ClarificationOptions
		g.logger.Info("reflex: disambiguation",
			"options", len(reflexResult.ClarificationOptions),
			"intent", reflexResult.Intent,
		)
	} else if g := strings.TrimSpace(reflexResult.Guidance); g != "" {
		guidance.WriteString("[reflex guidance]\n")
		guidance.WriteString(g)
		guidance.WriteString("\n\n")
	}

	// 1. Rules from reflex classification (semantic match).
	if len(reflexResult.MatchedRules) > 0 {
		matchedSet := make(map[string]bool, len(reflexResult.MatchedRules))
		for _, id := range reflexResult.MatchedRules {
			matchedSet[id] = true
		}
		for _, r := range rc.CandidateRules {
			if matchedSet[r.ID] && !seenRuleIDs[r.ID] {
				seenRuleIDs[r.ID] = true
				if !hasRules {
					guidance.WriteString("[active rules]\n")
					hasRules = true
				}
				fmt.Fprintf(&guidance, "WHEN: %s\nDO: %s\n\n", r.Trigger, r.Action)
				matchedRulesInfo = append(matchedRulesInfo, bs.MatchedRule{
					ID: r.ID, Trigger: r.Trigger, Action: r.Action, Source: "reflex",
				})
			}
		}
	}

	// 2. Rules from structured rule engine (condition-based match).
	var engineRuleCount int
	if us.Deps.RuleEngine != nil {
		engineRules := us.Deps.RuleEngine(ctx, bs.RuleContext{
			UserID:   us.Deps.UserID.String(),
			Intent:   reflexResult.Intent,
			Strategy: rc.Strategy,
			Hour:     time.Now().Hour(),
			Message:  msgText,
		})

		// Hard-silence gate: if any matched rule is marked Silent, abort the
		// turn entirely — no cortex call, no message sent. This is the only
		// way to enforce "do not respond" reliably; soft prompt instructions
		// in the rule's Action text are routinely ignored by the cortex LLM.
		for _, r := range engineRules {
			if r.Silent {
				g.logger.Info("rule engine: silent rule matched, aborting turn",
					"rule_id", r.ID,
					"trigger", r.Trigger,
					"chat_id", us.ChatID,
				)
				return reflexPipelineResult{Silent: true}
			}
		}

		for _, r := range engineRules {
			if seenRuleIDs[r.ID] {
				continue // already added by reflex
			}
			seenRuleIDs[r.ID] = true
			if !hasRules {
				guidance.WriteString("[active rules]\n")
				hasRules = true
			}
			fmt.Fprintf(&guidance, "WHEN: %s\nDO: %s\n\n", r.Trigger, r.Action)
			matchedRulesInfo = append(matchedRulesInfo, bs.MatchedRule{
				ID: r.ID, Trigger: r.Trigger, Action: r.Action, Source: "engine",
			})

			// Execute rule-prescribed pre_actions.
			for _, pa := range r.PreActions {
				paCtx, cancel := context.WithTimeout(ctx, preActionTimeout)
				result, isError := us.Registry.Execute(paCtx, pa.Tool, pa.Input)
				cancel()
				inputStr := string(pa.Input)
				if len(inputStr) > 200 {
					inputStr = inputStr[:200] + "..."
				}
				ruleOutputStr := result
				if len(ruleOutputStr) > 500 {
					ruleOutputStr = ruleOutputStr[:500] + "..."
				}
				preTraces = append(preTraces, agent.ToolTrace{Name: pa.Tool + " [rule]", Input: inputStr, Output: ruleOutputStr, Error: isError})
				if !isError {
					if researchBlock.Len() == 0 {
						researchBlock.WriteString("[research]\n")
					}
					fmt.Fprintf(&researchBlock, "[%s result]\n%s\n\n", pa.Tool, truncateStr(result, 2000))
				}
			}
		}
		engineRuleCount = len(engineRules)
		if engineRuleCount > 0 {
			g.logger.Info("rule engine matched", "count", engineRuleCount)
		}
	}

	if hasRules {
		guidance.WriteString("[/active rules]")
	}

	// Assemble: guidance (rules) + research + traces
	if researchBlock.Len() > 0 {
		guidance.WriteString("\n\n")
		guidance.WriteString(researchBlock.String())
	}

	// Intent-based guidance injection.
	if reflexResult.Intent == "memory_operation" && rc.ActiveNotes != "" && guidance.Len() == 0 {
		guidance.WriteString("[active_notes]\n")
		guidance.WriteString(rc.ActiveNotes)
		guidance.WriteString("[/active_notes]\n")
		guidance.WriteString("Если пользователь сообщает о выполнении — вызови memory_update(id, status=done).\n")
	}

	// Close research block if any pre-actions produced results.
	if researchBlock.Len() > 0 {
		researchBlock.WriteString("[/research]")
	}

	// When temporal_recall returned data, skip AME traces — they pollute
	// temporal queries with unrelated high-scoring memories from other dates.
	for _, pa := range preActionsToRun {
		if pa.Tool == "temporal_recall" && researchBlock.Len() > 50 {
			formattedTraces = ""
			break
		}
	}

	return reflexPipelineResult{
		InjectedCtx:     formattedTraces,
		ReflexGuidance:  guidance.String(),
		PostActions:     reflexResult.PostActions,
		PreTraces:       preTraces,
		EngineRuleCount: engineRuleCount,
		MemoriesCount:   rc.MemoriesCount,
		MatchedRules:    matchedRulesInfo,
		Strategy:        rc.Strategy,
	}
}

// escalateArgs is the parsed input of an escalate tool call.
type escalateArgs struct {
	Reason         string   `json:"reason"`
	Guidance       string   `json:"guidance"`
	SuggestedTools []string `json:"suggested_tools"`
}

// findEscalate scans tool traces for an escalate call. Detection keys on the
// tool name (never truncated); args are parsed best-effort — a truncated trace
// input still escalates, just without the guidance hint.
func findEscalate(traces []agent.ToolTrace) *escalateArgs {
	for _, tr := range traces {
		if tr.Name != tool.ToolEscalate {
			continue
		}
		var a escalateArgs
		_ = json.Unmarshal([]byte(tr.Input), &a)
		return &a
	}
	return nil
}

// runInteraction runs a turn through the two-tier interaction model when
// Gateway.InteractionTier is enabled, falling back to a direct Cortex call
// otherwise. It replaces the bare loop call in each of processMessages'
// transport blocks.
//
// Interaction-tier flow: the user message is persisted once, then the Reflex
// tier streams a reply. If Reflex answers directly (no escalate tool call)
// that reply is the turn. If Reflex calls escalate, its streamed text was a
// short spoken filler — the Cortex tier then runs and its answer becomes the
// turn. reflexCb is nil for non-voice transports so a filler is never shown
// as text; cortexCb drives the transport's own streaming (text deltas,
// tool_use / tool_result / thinking events for sinks that surface them).
func (g *Gateway) runInteraction(
	ctx context.Context,
	loop *agent.Loop,
	reflexLoop *agent.Loop,
	cortexCfg agent.RunConfig,
	reflexSystemPrompt string,
	content any,
	reflexCb, cortexCb *bs.StreamCallbacks,
	onReflexDone func(),
) (reply string, traces []agent.ToolTrace, escalated bool, err error) {
	// Legacy path: interaction tier off, or the caller didn't wire it up
	// (missing prompt / reflex loop / reflex system prompt).
	if !g.deps.Config.Gateway.InteractionTier || g.reflexInteractionPrompt == "" ||
		reflexLoop == nil || reflexSystemPrompt == "" {
		if g.deps.Config.Gateway.InteractionTier {
			g.logger.Warn("interaction tier enabled but not fully wired — running cortex directly")
		}
		reply, traces, err = loop.RunStream(ctx, cortexCfg, content, cortexCb)
		return reply, traces, false, err
	}

	// Heavy-content bypass: the reflex tier (openai-codex gpt-5.5) is
	// sized for short routing turns and chokes on either image bytes
	// (silently dropped by its text-only serializer) or oversized
	// text inputs (codex backend returns a confusing 400). PDFs and
	// large text-doc attachments inline as one huge text block in the
	// daemon's /chat handler and trip the size case. See
	// hasHeavyContent for the precise rule. Skip reflex and run
	// cortex directly with the full content; persist the user turn
	// once, matching the interaction-tier append-once pattern below.
	if hasHeavyContent(content) {
		if err = g.store.Append(ctx, cortexCfg.SessionID, bs.Message{
			Role:             "user",
			Content:          content,
			ReplyToMessageID: cortexCfg.ReplyToMessageID,
			TGMessageID:      cortexCfg.TGMessageID,
		}); err != nil {
			return "", nil, false, fmt.Errorf("interaction: append user message (heavy bypass): %w", err)
		}
		cortexCfg.SkipUserAppend = true
		g.logger.Info("interaction: heavy content, bypassing reflex tier", "session_id", cortexCfg.SessionID)
		reply, traces, err = loop.RunStream(ctx, cortexCfg, content, cortexCb)
		return reply, traces, true, err
	}

	// Phase 2 (calibration-backed): skip the reflex tier on TEXT transports —
	// those that wire no spoken-filler flush (onReflexDone == nil) — and run
	// Cortex directly, append-once (same pattern as the heavy bypass above).
	// Voice (onReflexDone != nil) keeps the two-tier reflex+filler. In text the
	// reflex pre-pass is net overhead: streaming Cortex is its own latency
	// handshake, and a cheap difficulty gate mis-routes ~half the hard turns.
	if onReflexDone == nil && g.deps.Config.Gateway.SkipReflexOnText {
		if err = g.store.Append(ctx, cortexCfg.SessionID, bs.Message{
			Role:             "user",
			Content:          content,
			ReplyToMessageID: cortexCfg.ReplyToMessageID,
			TGMessageID:      cortexCfg.TGMessageID,
		}); err != nil {
			return "", nil, false, fmt.Errorf("interaction: append user message (skip-reflex): %w", err)
		}
		cortexCfg.SkipUserAppend = true
		g.logger.Info("interaction: skip reflex on text, cortex direct", "session_id", cortexCfg.SessionID)
		reply, traces, err = loop.RunStream(ctx, cortexCfg, content, cortexCb)
		return reply, traces, false, err
	}

	// Derive the Reflex (interaction-tier) config from the Cortex config:
	// same session / context / rules, but the fast model, the reflex role
	// (escalate-only tools) and the focused interaction-tier system prompt
	// (preamble + persona; no cortex agents/tools manual).
	reflexCfg := cortexCfg
	reflexCfg.Model = g.reflexModel()
	reflexCfg.Role = "reflex"
	reflexCfg.SystemPrompt = reflexSystemPrompt
	reflexCfg.MaxTurns = 1
	reflexCfg.Ephemeral = true
	reflexCfg.SkipUserAppend = true
	reflexCfg.MaxTokens = 0
	reflexCfg.Temperature = 0
	// Tight history window for the fast tier — routing/answer decisions need
	// the recent conversation, not the full session. ~4 K tokens ≈ last 15-25
	// messages, enough for short-term continuity; full context lives on the
	// cortex side when escalation happens.
	reflexCfg.MessageBudget = 4000
	// AllowedTools cleared — reflex's only tool is the system `escalate`
	// sentinel, which must not be dropped by the per-soul cabinet allowlist.
	reflexCfg.AllowedTools = nil
	// Reasoning controls are NEVER inherited from cortex: reflex is
	// latency-critical and runs a different model. cortex's adaptive
	// thinking / xhigh effort copied via `reflexCfg := cortexCfg` would
	// 400 a model that lacks adaptive thinking (e.g. haiku) and otherwise
	// burn slow xhigh reasoning on a classifier. Cleared here, then set
	// from the reflex model_config row below.
	reflexCfg.ThinkingMode = ""
	reflexCfg.Effort = ""
	if g.deps.ModelStore != nil {
		ref := g.deps.ModelStore.Get("reflex")
		reflexCfg.MaxTokens = ref.MaxTokens
		reflexCfg.Temperature = ref.Temperature
		reflexCfg.ThinkingMode = ref.ThinkingMode
		reflexCfg.Effort = ref.Effort
		// Per-role thinking budget: 0 in DB = disabled. -1 forces the
		// agent loop's chooseThinkingBudget to ignore the global default.
		// Without this, reflex inherited cortex's 4096-token thinking
		// budget and gemma4-nothinker (and any thinking-capable model)
		// burned 400-500 hidden reasoning tokens per turn — ~5-6 s of
		// pure latency burn on the voice path.
		if ref.ThinkingBudget > 0 {
			reflexCfg.ThinkingBudget = ref.ThinkingBudget
		} else {
			reflexCfg.ThinkingBudget = -1
		}
	}

	// Persist the user message once; both tiers read it, neither re-appends.
	if err = g.store.Append(ctx, cortexCfg.SessionID, bs.Message{Role: "user", Content: content}); err != nil {
		return "", nil, false, fmt.Errorf("interaction: append user message: %w", err)
	}

	// Reflex runs against an escalate-only registry subset — if it
	// hallucinates a cortex tool (memory_search etc.) the registry rejects
	// it as unknown instead of executing it for real.
	reflexReply, reflexTraces, rerr := reflexLoop.RunStream(ctx, reflexCfg, content, reflexCb)
	if rerr != nil {
		return "", reflexTraces, false, fmt.Errorf("interaction: reflex: %w", rerr)
	}

	// Voice transports flush the reflex's spoken text-so-far the instant the
	// stream ends, so the filler reaches the user BEFORE cortex starts.
	if onReflexDone != nil {
		onReflexDone()
	}

	esc := findEscalate(reflexTraces)
	if esc == nil && len(reflexTraces) > 0 {
		// Reflex tried to call a tool but not (or not only) escalate — it
		// hallucinated a cortex tool. Any tool intent from the fast tier
		// means "I need the deep tier", so escalate. The hallucinated tool
		// name goes into the reason for observability.
		esc = &escalateArgs{Reason: "fast tier requested a tool: " + reflexTraces[0].Name}
		g.logger.Info("interaction: reflex called non-escalate tool, treating as escalation",
			"tool", reflexTraces[0].Name)
	}
	if esc == nil {
		// Simple turn — Reflex answered it. Persist its reply as the turn.
		if strings.TrimSpace(reflexReply) != "" {
			if aerr := g.store.Append(ctx, cortexCfg.SessionID, bs.Message{
				Role:    "assistant",
				Content: []bs.ContentBlock{{Type: "text", Text: reflexReply}},
			}); aerr != nil {
				g.logger.Warn("interaction: persist reflex reply failed", "error", aerr)
			}
		}
		g.logger.Info("interaction: reflex answered, no escalation")
		return reflexReply, reflexTraces, false, nil
	}

	// Escalation — run the Cortex tier with the full registry. The user
	// message is already persisted; Cortex persists its own answer normally.
	g.logger.Info("interaction: escalating to cortex", "reason", truncateStr(esc.Reason, 120))
	cortexCfg.SkipUserAppend = true
	if esc.Guidance != "" {
		note := "[escalation note] " + esc.Guidance
		if cortexCfg.ReflexGuidance != "" {
			cortexCfg.ReflexGuidance = note + "\n\n" + cortexCfg.ReflexGuidance
		} else {
			cortexCfg.ReflexGuidance = note
		}
	}
	reply, traces, err = loop.RunStream(ctx, cortexCfg, content, cortexCb)
	return reply, traces, true, err
}

// buildSinkCallbacks composes a *bs.StreamCallbacks from whichever optional
// streaming interfaces the sink implements. Returns nil if the sink supports
// no streaming at all (batch mode — only the final aggregated text reaches
// ResponseSink.SendText after the loop returns).
//
// Sinks that participate:
//   - TextStreamSink → cb.OnText forwards each delta as an SSE/WS frame.
//   - ToolUseSink → cb.OnToolUse / cb.OnToolResult render LLM tool calls.
//   - ThinkingSink → cb.OnThinking streams extended-thinking deltas.
func buildSinkCallbacks(ctx context.Context, sink bs.ResponseSink) *bs.StreamCallbacks {
	cb := &bs.StreamCallbacks{}
	any := false
	if ts, ok := sink.(bs.TextStreamSink); ok {
		cb.OnText = func(delta string) { _ = ts.SendTextDelta(ctx, delta) }
		any = true
	}
	if tu, ok := sink.(bs.ToolUseSink); ok {
		cb.OnToolUse = func(id, name string, input json.RawMessage) {
			_ = tu.SendToolUse(ctx, id, name, input)
		}
		cb.OnToolResult = func(useID, output string, isError bool, latencyMs int) {
			_ = tu.SendToolResult(ctx, useID, output, isError, latencyMs)
		}
		any = true
	}
	if th, ok := sink.(bs.ThinkingSink); ok {
		cb.OnThinking = func(delta string) { _ = th.SendThinking(ctx, delta) }
		any = true
	}
	if us, ok := sink.(bs.UsageSink); ok {
		cb.OnUsage = func(input, output int) { _ = us.SendUsage(ctx, input, output) }
		any = true
	}
	if !any {
		return nil
	}
	return cb
}

// BargeInEnabled reports whether the barge-in voice path is enabled. The
// WebSocket transport reads it to choose its connection-handling loop.
func (g *Gateway) BargeInEnabled() bool {
	return g.deps.Config.Gateway.BargeIn
}

// TranscribeAudio runs speech-to-text on raw audio bytes. Used by the barge-in
// turn manager to transcribe an interjection before classifying it.
func (g *Gateway) TranscribeAudio(ctx context.Context, audio []byte) (string, error) {
	if g.whisper == nil || !g.whisper.IsConfigured() {
		return "", fmt.Errorf("transcription not configured")
	}
	return g.whisper.Transcribe(ctx, audio, "voice.wav")
}

// ClassifyInterjection decides whether a user utterance that arrived mid-
// response is a backchannel (keep the turn running) or a real interruption
// (cancel it). It is a single cheap reflex-model call; it deliberately does
// not run the AME / rule pipeline so it stays fast and lock-free while the
// active turn is still streaming. inflightTail is what the assistant is
// currently saying — without it the classifier cannot tell "да-да, понятно"
// from "да-да, не то ищешь".
func (g *Gateway) ClassifyInterjection(ctx context.Context, transcript, inflightTail string) (bs.InterjectionClass, error) {
	model := g.reflexModel()
	if model == "" {
		return bs.InterjectionUnclear, fmt.Errorf("reflex model not configured")
	}
	if g.reflexInterjectionPrompt == "" {
		return bs.InterjectionUnclear, fmt.Errorf("reflex-interjection prompt not loaded")
	}

	// Only the recent tail of the in-flight response matters for the decision.
	tail := []rune(inflightTail)
	if len(tail) > 600 {
		tail = tail[len(tail)-600:]
	}
	prompt := fmt.Sprintf("Ассистент сейчас говорит:\n%s\n\nПользователь перебил репликой:\n%s",
		strings.TrimSpace(string(tail)), transcript)

	resp, err := g.provider.Complete(ctx, bs.CompletionRequest{
		Model:     model,
		MaxTokens: 16,
		System:    g.reflexInterjectionPrompt,
		Messages:  []bs.Message{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return bs.InterjectionUnclear, fmt.Errorf("classify interjection: %w", err)
	}

	out := strings.ToLower(strings.TrimSpace(bs.ExtractText(resp.Content)))
	switch {
	case strings.Contains(out, "interrupt"):
		return bs.InterjectionInterrupt, nil
	case strings.Contains(out, "backchannel"):
		return bs.InterjectionBackchannel, nil
	default:
		return bs.InterjectionUnclear, nil
	}
}

// PersistInterrupted records a cancelled turn's partial response as an
// assistant message so the session keeps user/assistant alternation intact —
// a dangling user message with no reply would break the next turn's API call.
// Runs on a fresh background context because the turn's own context is, by
// definition, already cancelled.
func (g *Gateway) PersistInterrupted(_ context.Context, chatID, partial string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	us, err := g.getOrInitUser(ctx, chatID)
	if err != nil {
		g.logger.Warn("persist interrupted: resolve user failed", "error", err)
		return
	}
	ctx = bs.WithSoulID(ctx, us.SoulID)

	us.Mu.Lock()
	defer us.Mu.Unlock()

	sess, err := g.GetOrCreateSession(ctx, us)
	if err != nil {
		g.logger.Warn("persist interrupted: session failed", "error", err)
		return
	}

	text := strings.TrimSpace(partial)
	if text == "" {
		text = "[прервано пользователем]"
	} else {
		text += " […прервано]"
	}
	if err := g.store.Append(ctx, sess.ID, bs.Message{
		Role:    "assistant",
		Content: []bs.ContentBlock{{Type: "text", Text: text}},
	}); err != nil {
		g.logger.Warn("persist interrupted: append failed", "error", err)
	}
}

// executePostActions runs post-cortex actions (save reflection, etc.).
