package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

func (g *Gateway) resolveTurnPolicy(
	ctx context.Context,
	us *UserState,
	registry *bs.ToolRegistry,
	userText string,
	ephemeral bool,
) (bs.TurnPolicy, []string) {
	if g == nil || g.deps == nil || g.deps.Config == nil || us == nil || registry == nil {
		return bs.TurnPolicy{EffectiveMode: bs.TurnPolicyOff}, nil
	}

	// Resolve the per-soul allowlist exactly once. This same snapshot drives
	// host policy availability, preflight denial, and the final Cortex
	// AllowedTools filter; a policy-store error fails closed for this turn.
	allowed, policyErr := g.resolveAllowedToolsForSoul(ctx, us.SoulID, registry)
	if policyErr != nil {
		g.logger.Error("soul tools: policy lookup failed, failing turn tools closed",
			"soul_id", us.SoulID.String(), "error", policyErr)
		allowed = []string{}
	}

	// MCP tools are attached per soul at turn time rather than to the durable
	// user registry. Resolve policy against that same concrete surface so a
	// mixed self-history + connector request is not silently dropped.
	base := g.baseToolNamesForRole(registry, "cortex")
	available := availableToolNames(registry, base, allowed)
	if g.deps.Config.TurnPolicyResolver == nil {
		policy := bs.TurnPolicy{EffectiveMode: bs.TurnPolicyOff}
		if policyErr != nil {
			policy = failClosedSoulToolPolicy(policy, registry)
		}
		return policy, allowed
	}
	req := bs.TurnPolicyRequest{
		Role:           "cortex",
		UserID:         us.UserID.String(),
		SoulID:         us.SoulID.String(),
		UserText:       userText,
		BaseTools:      cloneStrings(base),
		AvailableTools: cloneStrings(available),
		Ephemeral:      ephemeral,
	}
	policy := g.deps.Config.TurnPolicyResolver(ctx, req)
	policy.Tools = concreteTurnPolicyTools(registry, policy.Tools, available)
	policy = normalizeTurnPolicy(policy, available)
	if policyErr != nil {
		policy = failClosedSoulToolPolicy(policy, registry)
	}
	return policy, allowed
}

func failClosedSoulToolPolicy(policy bs.TurnPolicy, registry *bs.ToolRegistry) bs.TurnPolicy {
	policy.EffectiveMode = bs.TurnPolicyOff
	policy.Tools = nil
	policy.Strict = false
	policy.Guidance = ""
	policy.PreActions = nil
	policy.ObserverActions = nil
	policy.SuppressToolDirectives = false
	policy.UnavailableReason = "soul_tool_policy_unavailable"
	if policy.Diagnostics == nil {
		policy.Diagnostics = make(map[string]string)
	}
	policy.Diagnostics["tool_policy_status"] = "unavailable"
	if registry != nil {
		for _, definition := range registry.Definitions() {
			policy.DeniedTools = append(policy.DeniedTools, definition.Name)
		}
	}
	policy.DeniedTools = dedupeStrings(policy.DeniedTools)
	return policy
}

func availableToolNames(registry *bs.ToolRegistry, base, allowed []string) []string {
	defs := registry.DefinitionsForNames(base)
	allowedSet := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = true
	}
	out := make([]string, 0, len(defs))
	availablePeers := make(map[string]bool)
	for _, def := range defs {
		if allowed != nil && !allowedSet[def.Name] {
			continue
		}
		out = append(out, def.Name)
		if peer := registry.PeerForTool(def.Name); peer != "" {
			availablePeers[peer] = true
		}
	}
	// Hosts select MCP packs through role-level peer wildcards. Preserve a
	// wildcard only when at least one concrete, allowed tool backs it.
	for _, name := range base {
		if !strings.HasPrefix(name, "peer:") {
			continue
		}
		if availablePeers[strings.TrimPrefix(name, "peer:")] {
			out = append(out, name)
		}
	}
	return dedupeStrings(out)
}

func concreteTurnPolicyTools(registry *bs.ToolRegistry, selected, available []string) []string {
	if len(selected) == 0 {
		return selected
	}
	availableSet := make(map[string]bool, len(available))
	for _, name := range available {
		availableSet[name] = true
	}
	out := make([]string, 0, len(selected))
	for _, name := range selected {
		if !strings.HasPrefix(name, "peer:") {
			out = append(out, name)
			continue
		}
		before := len(out)
		for _, def := range registry.DefinitionsForNames([]string{name}) {
			if availableSet[def.Name] {
				out = append(out, def.Name)
			}
		}
		if len(out) == before {
			// Keep an unresolved wildcard so normalizeTurnPolicy fails closed
			// rather than silently deleting a host-required pack.
			out = append(out, name)
		}
	}
	return dedupeStrings(out)
}

func normalizeTurnPolicy(policy bs.TurnPolicy, available []string) bs.TurnPolicy {
	switch policy.EffectiveMode {
	case bs.TurnPolicyOff, bs.TurnPolicyShadow, bs.TurnPolicyCanary, bs.TurnPolicyOn:
	default:
		policy.EffectiveMode = bs.TurnPolicyOff
		policy.UnavailableReason = "invalid_effective_mode"
	}
	if !policy.Active() {
		policy.Tools = nil
		policy.Strict = false
		policy.Guidance = ""
		policy.PreActions = nil
		policy.SuppressToolDirectives = false
		if policy.EffectiveMode != bs.TurnPolicyShadow {
			policy.ObserverActions = nil
		}
		return policy
	}

	availableSet := make(map[string]bool, len(available))
	for _, name := range available {
		availableSet[name] = true
	}
	required := append([]string(nil), policy.Tools...)
	for _, action := range policy.PreActions {
		required = append(required, action.Tool)
	}
	for _, name := range dedupeStrings(required) {
		if availableSet[name] {
			continue
		}
		policy.DeniedTools = dedupeStrings(append(policy.DeniedTools, name))
		policy.EffectiveMode = bs.TurnPolicyOff
		policy.Tools = nil
		policy.Strict = false
		policy.Guidance = ""
		policy.PreActions = nil
		policy.ObserverActions = nil
		policy.SuppressToolDirectives = false
		policy.UnavailableReason = "required_tool_unavailable:" + name
		break
	}
	return policy
}

func (g *Gateway) startTurnPolicyObservers(ctx context.Context, us *UserState, policy bs.TurnPolicy) {
	if policy.EffectiveMode != bs.TurnPolicyShadow ||
		len(policy.ObserverActions) == 0 || us == nil || us.Registry == nil {
		return
	}
	actions := append([]bs.ToolAction(nil), policy.ObserverActions...)
	go func() {
		observerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
		defer cancel()
		for _, action := range actions {
			started := time.Now()
			result, isError := us.Registry.Execute(observerCtx, action.Tool, action.Input)
			g.logger.Info("turn policy shadow observer",
				"source", policy.Source,
				"tool", action.Tool,
				"is_error", isError,
				"result_len", len(result),
				"latency_ms", time.Since(started).Milliseconds(),
			)
			if observerCtx.Err() != nil {
				return
			}
		}
	}()
}

func (g *Gateway) runTurnPolicyPreActions(
	ctx context.Context,
	us *UserState,
	policy bs.TurnPolicy,
	timings *turnTimer,
) (string, []agent.ToolTrace) {
	if !policy.Active() {
		return "", nil
	}
	var guidance strings.Builder
	if text := strings.TrimSpace(policy.Guidance); text != "" {
		guidance.WriteString(text)
		guidance.WriteString("\n")
	}
	if len(policy.PreActions) == 0 || us == nil || us.Registry == nil {
		return guidance.String(), nil
	}

	// One wall-clock budget covers every required evidence action. Individual
	// tools must honor cancellation; a timeout result stays evidence of an
	// unavailable check and can never become a negative historical claim.
	actionCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	guidance.WriteString("\n[required evidence]\n")
	traces := make([]agent.ToolTrace, 0, len(policy.PreActions))
	for _, action := range policy.PreActions {
		started := time.Now()
		type actionResult struct {
			output  string
			isError bool
		}
		done := make(chan actionResult, 1)
		go func(action bs.ToolAction) {
			output, isError := us.Registry.Execute(actionCtx, action.Tool, action.Input)
			done <- actionResult{output: output, isError: isError}
		}(action)
		var result string
		var isError bool
		select {
		case completed := <-done:
			result, isError = completed.output, completed.isError
		case <-actionCtx.Done():
			result = actionCtx.Err().Error()
			isError = true
		}
		if timings != nil {
			timings.RecordSince("turn_policy.pre_action", started,
				fmt.Sprintf("tool=%s error=%t", action.Tool, isError))
		}
		// Truncate by runes: evidence inputs and results are natural language,
		// so a byte cut lands mid-codepoint and hands the debug trace broken
		// UTF-8 instead of a readable prefix.
		input := string(action.Input)
		if bounded := truncateStr(input, 200); bounded != input {
			input = bounded + "..."
		}
		traceOutput := result
		if bounded := truncateStr(traceOutput, 500); bounded != traceOutput {
			traceOutput = bounded + "..."
		}
		traces = append(traces, agent.ToolTrace{
			Name:   action.Tool,
			Input:  input,
			Output: traceOutput,
			Error:  isError,
		})
		if isError {
			fmt.Fprintf(&guidance,
				"[%s error]\nThe required evidence check failed. This is not found evidence and cannot support a historical conclusion.\n%s\n\n",
				action.Tool, truncateStr(result, 12000))
		} else {
			fmt.Fprintf(&guidance, "[%s result]\n%s\n\n", action.Tool, truncateStr(result, 12000))
		}
		if actionCtx.Err() != nil {
			break
		}
	}
	guidance.WriteString("[/required evidence]\n")
	return guidance.String(), traces
}

func applyTurnPolicy(cfg *agent.RunConfig, policy bs.TurnPolicy) {
	if cfg == nil {
		return
	}
	cfg.DeniedTools = dedupeStrings(append(cfg.DeniedTools, policy.DeniedTools...))
	cfg.TurnPolicyActive = policy.RequireCortex
	if !policy.Active() {
		return
	}
	cfg.ToolOverride = cloneStrings(policy.Tools)
	cfg.ToolboxExpansion = nil
	cfg.StrictTools = policy.Strict
	cfg.TurnPolicyActive = true
	cfg.ToolSelectionSource = policy.Source
}

func (g *Gateway) applyTurnToolPolicy(
	ctx context.Context,
	registry *bs.ToolRegistry,
	cfg *agent.RunConfig,
	userText string,
	forcedTools []string,
	ephemeral bool,
	policy bs.TurnPolicy,
) {
	if !ephemeral {
		g.applyToolSelector(ctx, registry, cfg, userText, forcedTools, false)
	}
	// Denials are safety policy, not model-facing tool selection. They must
	// apply to ephemeral/notebook turns even though those turns deliberately
	// skip the ordinary ToolSelector.
	applyTurnPolicy(cfg, policy)
}

func prependTurnGuidance(prefix, existing string) string {
	prefix = strings.TrimSpace(prefix)
	existing = strings.TrimSpace(existing)
	switch {
	case prefix == "":
		return existing
	case existing == "":
		return prefix
	default:
		return prefix + "\n\n" + existing
	}
}

func ruleHasToolDirective(rule bs.ActiveRule) bool {
	return len(rule.Tools) > 0 || len(rule.PreActions) > 0
}

func suppressToolRule(rule bs.ActiveRule) bs.ActiveRule {
	rule.Suppressed = true
	rule.SuppressedReason = "turn_policy_tool_directives_suppressed"
	rule.Disposition = "suppressed"
	return rule
}

func semanticRuleHasToolDirective(rule bs.CandidateRule, registry *bs.ToolRegistry) bool {
	action := strings.ToLower(strings.TrimSpace(rule.Action))
	if action == "" {
		return false
	}
	if strings.Contains(action, "call immediately") {
		return true
	}

	knownTools := make(map[string]bool)
	if registry != nil {
		for _, def := range registry.Definitions() {
			knownTools[strings.ToLower(def.Name)] = true
		}
	}
	words := strings.FieldsFunc(action, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'а' && r <= 'я') || r == 'ё' ||
			(r >= '0' && r <= '9') || r == '_')
	})
	directiveVerb := func(word string) bool {
		switch word {
		case "call", "invoke", "use", "execute", "run",
			"вызови", "вызывай", "используй", "использовать",
			"выполни", "выполняй", "выполнить":
			return true
		default:
			return false
		}
	}
	filler := func(word string) bool {
		switch word {
		case "a", "an", "the", "this", "selected", "immediately", "сразу", "этот", "нужный":
			return true
		default:
			return false
		}
	}
	for i, word := range words {
		if !directiveVerb(word) {
			continue
		}
		checked := 0
		for j := i + 1; j < len(words) && checked < 3; j++ {
			if filler(words[j]) {
				continue
			}
			checked++
			if knownTools[words[j]] || strings.Contains(words[j], "_") {
				return true
			}
			switch words[j] {
			case "tool", "tools", "инструмент", "инструменты", "mcp":
				return true
			}
		}
	}
	return false
}

func suppressedSemanticRule(rule bs.CandidateRule) bs.MatchedRule {
	return bs.MatchedRule{
		ID:               rule.ID,
		Trigger:          rule.Trigger,
		Action:           rule.Action,
		Source:           "reflex",
		MatchType:        "semantic",
		Reason:           "semantic candidate contains a tool directive",
		Disposition:      "suppressed",
		Suppressed:       true,
		SuppressedReason: "turn_policy_tool_directives_suppressed",
	}
}
