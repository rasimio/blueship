package gateway

import (
	"fmt"
	"strings"

	"github.com/rasimio/blueship/runtime/agent"
)

func (g *Gateway) logCortexTurnAnatomy(cfg agent.RunConfig, forcedTools []string) {
	if g == nil || g.logger == nil {
		return
	}
	turnContext := strings.TrimSpace(strings.Join([]string{cfg.ReflexGuidance, cfg.InjectedContext}, "\n\n"))
	g.logger.Info("cortex turn context anatomy",
		"session_id", cfg.SessionID,
		"role", cfg.Role,
		"ephemeral", cfg.Ephemeral,
		"message_budget", cfg.MessageBudget,
		"message_budget_source", cfg.MessageBudgetSource,
		"injected_context_chars", len(cfg.InjectedContext),
		"injected_context_runes", len([]rune(cfg.InjectedContext)),
		"injected_context_tokens_estimate", estimateGatewayTextTokens(cfg.InjectedContext),
		"reflex_guidance_chars", len(cfg.ReflexGuidance),
		"reflex_guidance_runes", len([]rune(cfg.ReflexGuidance)),
		"reflex_guidance_tokens_estimate", estimateGatewayTextTokens(cfg.ReflexGuidance),
		"turn_context_chars", len(turnContext),
		"turn_context_runes", len([]rune(turnContext)),
		"turn_context_tokens_estimate", estimateGatewayTextTokens(turnContext),
		"turn_context_sections", gatewaySectionCounts(turnContext),
		"forced_tools_count", len(forcedTools),
		"forced_tools", joinGatewayValues(dedupeStrings(forcedTools), 40),
		"tool_override_set", cfg.ToolOverride != nil,
		"tool_override_count", len(cfg.ToolOverride),
		"tool_override", joinGatewayValues(cfg.ToolOverride, 40),
		"allowed_tools_count", len(cfg.AllowedTools),
	)
}

func estimateGatewayTextTokens(s string) int {
	if s == "" {
		return 0
	}
	return len([]rune(s)) / 3
}

func gatewaySectionCounts(text string) string {
	counts := map[string]int{
		"active_rules":    strings.Count(text, "[active rules]"),
		"research":        strings.Count(text, "[research]"),
		"active_notes":    strings.Count(text, "[active_notes]"),
		"memory":          strings.Count(text, "[memory"),
		"insight":         strings.Count(text, "[insight"),
		"episode":         strings.Count(text, "[episode"),
		"verbatim":        strings.Count(text, "[verbatim"),
		"stance":          strings.Count(text, "[stance"),
		"relation":        strings.Count(text, "[relation"),
		"rule":            strings.Count(text, "[rule"),
		"retrieval":       strings.Count(text, "[retrieval]"),
		"temporal_anchor": strings.Count(text, "[temporal_anchor]"),
		"contradictions":  strings.Count(text, "[contradictions]"),
	}
	return orderedGatewayCountString(counts, []string{
		"active_rules", "research", "active_notes", "memory", "insight", "episode",
		"verbatim", "stance", "relation", "rule", "retrieval", "temporal_anchor", "contradictions",
	})
}

func joinGatewayValues(values []string, limit int) string {
	if len(values) == 0 {
		return ""
	}
	if limit <= 0 || len(values) <= limit {
		return strings.Join(values, ",")
	}
	clipped := append([]string{}, values[:limit]...)
	clipped = append(clipped, fmt.Sprintf("+%d_more", len(values)-limit))
	return strings.Join(clipped, ",")
}

func orderedGatewayCountString(counts map[string]int, order []string) string {
	if len(counts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(counts))
	for _, key := range order {
		if value := counts[key]; value > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", key, value))
		}
	}
	return strings.Join(parts, ",")
}
