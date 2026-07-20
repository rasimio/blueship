package gateway

import (
	"context"
	"strings"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

func (g *Gateway) applyToolSelector(
	ctx context.Context,
	registry *bs.ToolRegistry,
	cfg *agent.RunConfig,
	userText string,
	forcedTools []string,
	ephemeral bool,
) {
	if cfg == nil || registry == nil || g.deps == nil || g.deps.Config == nil || g.deps.Config.ToolSelector == nil {
		return
	}
	baseTools := g.baseToolNamesForRole(registry, cfg.Role)
	selection := g.deps.Config.ToolSelector(ctx, bs.ToolSelectionRequest{
		Role:            cfg.Role,
		UserText:        userText,
		BaseTools:       cloneStrings(baseTools),
		ForcedTools:     dedupeStrings(forcedTools),
		ReflexGuidance:  cfg.ReflexGuidance,
		InjectedContext: cfg.InjectedContext,
		Ephemeral:       ephemeral,
	})
	if selection.Tools == nil {
		return
	}
	cfg.ToolOverride = g.filterSelectedToolsToRole(registry, baseTools, selection.Tools)
	cfg.ToolSelectionSource = selection.Source
	// Arm the open_toolbox escape hatch: whenever a selector narrows the
	// toolset, the loop offers a synthetic tool that unlocks the full role
	// list mid-turn. A selector miss then costs one extra round-trip, not
	// the whole turn (see agent.RunConfig.ToolboxExpansion).
	cfg.ToolboxExpansion = cloneStrings(baseTools)
	g.logger.Info("tool selector applied",
		"role", cfg.Role,
		"source", cfg.ToolSelectionSource,
		"selected", len(cfg.ToolOverride),
		"forced", len(forcedTools),
	)
}

func (g *Gateway) baseToolNamesForRole(registry *bs.ToolRegistry, role string) []string {
	if role != "" && g.deps != nil && g.deps.RoleTools != nil {
		if names := g.deps.RoleTools.Get(role); names != nil {
			return cloneStrings(names)
		}
	}
	defs := registry.Definitions()
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	return names
}

func (g *Gateway) filterSelectedToolsToRole(registry *bs.ToolRegistry, baseTools, selected []string) []string {
	if len(selected) == 0 {
		return []string{}
	}
	baseSet := make(map[string]bool)
	basePeer := make(map[string]bool)
	for _, def := range registry.DefinitionsForNames(baseTools) {
		baseSet[def.Name] = true
	}
	for _, name := range baseTools {
		if strings.HasPrefix(name, "peer:") {
			basePeer[name] = true
		}
	}

	out := make([]string, 0, len(selected))
	seen := make(map[string]bool, len(selected))
	for _, name := range selected {
		if name == "" || seen[name] {
			continue
		}
		if strings.HasPrefix(name, "peer:") {
			if !basePeer[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
			continue
		}
		if !baseSet[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
