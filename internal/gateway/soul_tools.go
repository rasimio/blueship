package gateway

import (
	"context"
	"fmt"

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
	allowed, err := g.resolveAllowedToolsForSoul(ctx, soulID, registry)
	if err != nil {
		g.logger.Warn("soul tools: policy lookup failed, allowing all tools",
			"soul_id", soulID.String(), "error", err)
		return nil
	}
	return allowed
}

// resolveAllowedToolsForSoul is the strict counterpart used by direct tool
// invocation. Chat keeps its historical fail-open behavior through
// allowedToolsForSoul, while an explicit API call must fail closed when the
// host's policy store is unavailable.
func (g *Gateway) resolveAllowedToolsForSoul(ctx context.Context, soulID uuid.UUID, registry *bs.ToolRegistry) ([]string, error) {
	if soulID == uuid.Nil || registry == nil {
		return nil, nil
	}
	policy := g.deps.Config.Gateway.ResolveSoulToolPolicy
	if policy == nil {
		return nil, nil
	}
	override, providers, err := policy(ctx, soulID)
	if err != nil {
		return nil, fmt.Errorf("resolve soul tool policy: %w", err)
	}
	connected := make(map[string]bool, len(providers))
	for _, p := range providers {
		connected[p] = true
	}

	meta := g.deps.Config.ToolMeta
	// A configured policy that allows zero tools must remain distinguishable
	// from nil ("no policy configured").
	allowed := make([]string, 0, len(registry.Definitions()))
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
	return allowed, nil
}
