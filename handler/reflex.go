package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rasimio/blueship/core"
)

const (
	reflexConfidenceThreshold = 0.7
	preActionTimeout          = 10 * time.Second
	maxPreActions             = 2
)

// reflexResult holds the output of a reflex pipeline run for agent tasks.
type reflexResult struct {
	InjectedCtx string // AME traces (formatted)
	Guidance    string // [active rules] + [research] blocks
}

// runReflexPipeline executes the System 1/2 pipeline for agent tasks.
// Same architecture as gateway.runReflexPipeline but independent of Gateway.
func runReflexPipeline(ctx context.Context, deps core.AgentDeps, tz *time.Location, msg string) reflexResult {
	if deps.ReflexPreparer == nil {
		// No reflex — fall back to ContextInjector + RuleEngine only.
		return runFallbackPipeline(ctx, deps, tz, msg)
	}

	rc := deps.ReflexPreparer(ctx, deps.UserID.String(), msg)
	if rc == nil {
		return runFallbackPipeline(ctx, deps, tz, msg)
	}

	// Build reflex prompt.
	var rulesBlock strings.Builder
	for _, r := range rc.CandidateRules {
		fmt.Fprintf(&rulesBlock, "[%s] WHEN: %s → DO: %s (sr=%.0f%%)\n",
			r.ID, r.Trigger, r.Action, r.SuccessRate*100)
	}

	// Tool list with descriptions (same format as gateway).
	toolsList := buildToolsList(deps)

	// Load reflex prompts from DB.
	reflexPlanTemplate, _ := deps.Prompts.Get(ctx, "reflex-plan")
	if reflexPlanTemplate == "" {
		deps.Logger.Warn("reflex-plan prompt not in DB, using fallback")
		return reflexResult{
			InjectedCtx: rc.FullContext,
			Guidance:    buildRuleEngineGuidance(ctx, deps, tz, msg),
		}
	}

	reflexSystemPrompt, _ := deps.Prompts.Get(ctx, "reflex-system")
	now := time.Now().In(tz)
	reflexSystem := fmt.Sprintf("[current_datetime: %s]\n\n%s",
		now.Format("2006-01-02 15:04 MST (Monday)"), reflexSystemPrompt)

	notesBlock := rc.ActiveNotes
	if notesBlock == "" {
		notesBlock = "(нет активных заметок)"
	}
	reflexPrompt := fmt.Sprintf(reflexPlanTemplate, rulesBlock.String(), toolsList, notesBlock, msg)

	// Resolve reflex model.
	reflexModel := ""
	if deps.ModelStore != nil {
		reflexModel = deps.ModelStore.ForRouter("reflex")
	}
	if reflexModel == "" {
		deps.Logger.Warn("reflex model not configured, using fallback")
		return reflexResult{
			InjectedCtx: rc.FullContext,
			Guidance:    buildRuleEngineGuidance(ctx, deps, tz, msg),
		}
	}

	// Call reflex LLM.
	deps.Logger.Info("agent-tasks: calling reflex", "model", reflexModel)
	resp, err := deps.LLM.Complete(ctx, core.CompletionRequest{
		Model:     reflexModel,
		MaxTokens: 512,
		System:    reflexSystem,
		Messages:  []core.Message{{Role: "user", Content: reflexPrompt}},
	})
	if err != nil {
		deps.Logger.Warn("agent-tasks: reflex failed, using fallback", "error", err)
		return reflexResult{
			InjectedCtx: rc.FullContext,
			Guidance:    buildRuleEngineGuidance(ctx, deps, tz, msg),
		}
	}

	reflexOutput, err := parseReflexResult(core.ExtractText(resp.Content))
	if err != nil {
		deps.Logger.Warn("agent-tasks: reflex parse failed", "error", err)
		return reflexResult{
			InjectedCtx: rc.FullContext,
			Guidance:    buildRuleEngineGuidance(ctx, deps, tz, msg),
		}
	}

	deps.Logger.Info("agent-tasks: reflex plan",
		"intent", reflexOutput.Intent,
		"confidence", reflexOutput.Confidence,
		"matched_rules", reflexOutput.MatchedRules,
		"pre_actions", len(reflexOutput.PreActions),
		"tools", reflexOutput.Tools,
	)

	// Low confidence → use full context but keep rule engine.
	lowConfidence := reflexOutput.Confidence < reflexConfidenceThreshold
	if lowConfidence {
		reflexOutput.MatchedRules = nil
		reflexOutput.PreActions = nil
	}
	formattedTraces := rc.FormattedTraces
	if lowConfidence {
		formattedTraces = rc.FullContext
	}

	// Execute pre-actions.
	var researchBlock strings.Builder
	preActionsToRun := reflexOutput.PreActions
	if len(preActionsToRun) > maxPreActions {
		preActionsToRun = preActionsToRun[:maxPreActions]
	}
	for _, pa := range preActionsToRun {
		paCtx, cancel := context.WithTimeout(ctx, preActionTimeout)
		result, isError := deps.Registry.Execute(paCtx, pa.Tool, pa.Input)
		cancel()
		if isError {
			deps.Logger.Warn("agent-tasks: reflex pre-action failed", "tool", pa.Tool)
			continue
		}
		if researchBlock.Len() == 0 {
			researchBlock.WriteString("[research]\n")
		}
		if len(result) > 2000 {
			result = result[:2000]
		}
		fmt.Fprintf(&researchBlock, "[%s result]\n%s\n\n", pa.Tool, result)
	}

	// Build guidance from reflex matched rules + rule engine.
	var guidance strings.Builder
	seenRuleIDs := make(map[string]bool)
	hasRules := false

	// Reflex guidance.
	if g := strings.TrimSpace(reflexOutput.Guidance); g != "" {
		guidance.WriteString("[reflex guidance]\n")
		guidance.WriteString(g)
		guidance.WriteString("\n\n")
	}

	// Reflex-matched rules (semantic).
	if len(reflexOutput.MatchedRules) > 0 {
		matchedSet := make(map[string]bool, len(reflexOutput.MatchedRules))
		for _, id := range reflexOutput.MatchedRules {
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
			}
		}
	}

	// Rule Engine (structured conditions).
	if deps.RuleEngine != nil {
		engineRules := deps.RuleEngine(ctx, core.RuleContext{
			UserID:   deps.UserID.String(),
			Intent:   reflexOutput.Intent,
			Strategy: rc.Strategy,
			Hour:     now.Hour(),
			Message:  msg,
		})

		for _, r := range engineRules {
			if seenRuleIDs[r.ID] {
				continue
			}
			seenRuleIDs[r.ID] = true
			if !hasRules {
				guidance.WriteString("[active rules]\n")
				hasRules = true
			}
			fmt.Fprintf(&guidance, "WHEN: %s\nDO: %s\n\n", r.Trigger, r.Action)

			// Rule pre-actions.
			for _, pa := range r.PreActions {
				paCtx, cancel := context.WithTimeout(ctx, preActionTimeout)
				result, isError := deps.Registry.Execute(paCtx, pa.Tool, pa.Input)
				cancel()
				if !isError {
					if researchBlock.Len() == 0 {
						researchBlock.WriteString("[research]\n")
					}
					if len(result) > 2000 {
						result = result[:2000]
					}
					fmt.Fprintf(&researchBlock, "[%s result]\n%s\n\n", pa.Tool, result)
				}
			}
		}
	}

	if hasRules {
		guidance.WriteString("[/active rules]")
	}
	if researchBlock.Len() > 0 {
		guidance.WriteString("\n\n")
		guidance.WriteString(researchBlock.String())
	}

	return reflexResult{
		InjectedCtx: formattedTraces,
		Guidance:    guidance.String(),
	}
}

// runFallbackPipeline uses ContextInjector + RuleEngine when reflex is unavailable.
func runFallbackPipeline(ctx context.Context, deps core.AgentDeps, tz *time.Location, msg string) reflexResult {
	var injectedCtx string
	if deps.ContextInjector != nil {
		injectedCtx = deps.ContextInjector(ctx, deps.UserID.String(), msg)
	}
	guidance := buildRuleEngineGuidance(ctx, deps, tz, msg)
	if guidance != "" {
		if injectedCtx != "" {
			injectedCtx += "\n\n" + guidance
		} else {
			injectedCtx = guidance
		}
		return reflexResult{InjectedCtx: injectedCtx}
	}
	return reflexResult{InjectedCtx: injectedCtx}
}

// buildRuleEngineGuidance runs Rule Engine only (no reflex LLM).
func buildRuleEngineGuidance(ctx context.Context, deps core.AgentDeps, tz *time.Location, msg string) string {
	if deps.RuleEngine == nil {
		return ""
	}
	now := time.Now().In(tz)
	engineRules := deps.RuleEngine(ctx, core.RuleContext{
		UserID:  deps.UserID.String(),
		Hour:    now.Hour(),
		Message: msg,
	})
	if len(engineRules) == 0 {
		return ""
	}
	var rb strings.Builder
	rb.WriteString("[active rules]\n")
	for _, r := range engineRules {
		fmt.Fprintf(&rb, "WHEN: %s\nDO: %s\n\n", r.Trigger, r.Action)
	}
	rb.WriteString("[/active rules]")
	return rb.String()
}

// buildToolsList formats tool descriptions for the reflex prompt.
func buildToolsList(deps core.AgentDeps) string {
	if deps.Registry == nil || deps.RoleTools == nil {
		return "none configured"
	}
	names := deps.RoleTools.Get("cortex")
	if len(names) == 0 {
		return "none configured"
	}

	local := &strings.Builder{}
	peerTools := make(map[string]*strings.Builder)

	for _, def := range deps.Registry.DefinitionsForNames(names) {
		peer := deps.Registry.PeerForTool(def.Name)
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
		sb.WriteString("[local tools]\n")
		sb.WriteString(local.String())
	}
	for peer, buf := range peerTools {
		fmt.Fprintf(&sb, "\n[%s tools]\n", peer)
		sb.WriteString(buf.String())
	}
	if sb.Len() == 0 {
		return "none configured"
	}
	return strings.TrimRight(sb.String(), "\n")
}

// parseReflexResult parses the JSON output from the reflex LLM.
func parseReflexResult(text string) (*core.ReflexResult, error) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		if len(lines) > 2 {
			text = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var raw struct {
		MatchedRules         []string                   `json:"matched_rules"`
		Intent               string                     `json:"intent"`
		Confidence           float64                    `json:"confidence"`
		PreActions           []core.ToolAction          `json:"pre_actions"`
		PostActions          []core.PostAction          `json:"post_actions"`
		Tools                json.RawMessage            `json:"tools"`
		Guidance             string                     `json:"guidance"`
		ClarificationOptions []core.ClarificationOption `json:"clarification_options"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("parse reflex JSON: %w", err)
	}

	result := &core.ReflexResult{
		MatchedRules:         raw.MatchedRules,
		Intent:               raw.Intent,
		Confidence:           raw.Confidence,
		PreActions:           raw.PreActions,
		PostActions:          raw.PostActions,
		Guidance:             raw.Guidance,
		ClarificationOptions: raw.ClarificationOptions,
	}

	if len(raw.Tools) > 0 {
		var toolStrings []string
		if err := json.Unmarshal(raw.Tools, &toolStrings); err == nil {
			result.Tools = toolStrings
		} else {
			var toolObjects []struct{ Tool string `json:"tool"` }
			if err := json.Unmarshal(raw.Tools, &toolObjects); err == nil {
				for _, t := range toolObjects {
					result.Tools = append(result.Tools, t.Tool)
				}
			}
		}
	}

	return result, nil
}

