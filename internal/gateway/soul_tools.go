package gateway

import (
	"context"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

// allowedToolsForSoul returns the set of tool names a soul may use: every
// core tool (internal machinery — always on), every non-core tool not
// disabled by the host's per-soul policy, and a provider-bound tool only when
// the soul has connected that provider. The result is handed to the agent loop
// as RunConfig.AllowedTools — a hard per-turn allowlist, so a cabinet toggle or
// a freshly connected integration takes effect on the very next message.
// registry is the per-turn registry (native + this soul's MCP tools).
//
// Returns nil — meaning "no filtering, every registered tool is available" —
// for a nil soul, when no ResolveSoulToolPolicy hook is configured (framework
// consumers outside a platform model), and on any policy error (a config-store
// blip must never strand the soul).
func (g *Gateway) allowedToolsForSoul(ctx context.Context, soulID uuid.UUID, registry *bs.ToolRegistry) []string {
	if soulID == uuid.Nil || registry == nil {
		return nil
	}
	policy := g.deps.Config.Gateway.ResolveSoulToolPolicy
	if policy == nil {
		return nil
	}
	override, providers, err := policy(ctx, soulID)
	if err != nil {
		g.logger.Warn("soul tools: policy lookup failed, allowing all tools",
			"soul_id", soulID.String(), "error", err)
		return nil
	}
	connected := make(map[string]bool, len(providers))
	for _, p := range providers {
		connected[p] = true
	}

	meta := g.deps.Config.ToolMeta
	var allowed []string
	for _, def := range registry.Definitions() {
		name := def.Name
		m := meta[name]
		// A provider-bound tool is offered only once that provider is connected.
		if m.Provider != "" && !connected[m.Provider] {
			continue
		}
		if m.Core {
			allowed = append(allowed, name) // core machinery — always on
			continue
		}
		if enabled, ok := override[name]; ok {
			if enabled {
				allowed = append(allowed, name)
			}
			continue
		}
		allowed = append(allowed, name) // no explicit choice — default on
	}
	return allowed
}
