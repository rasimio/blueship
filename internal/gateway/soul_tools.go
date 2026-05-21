package gateway

import (
	"context"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/core"
)

// allowedToolsForSoul returns the set of tool names a soul may use: every
// core tool (internal machinery — always on), every non-core tool not
// disabled in vaelum.soul_tools, and a provider-bound tool only when the
// soul has connected that provider. The result is handed to the agent loop
// as RunConfig.AllowedTools — a hard per-turn allowlist, so a cabinet
// toggle or a freshly connected integration takes effect on the very next
// message. registry is the per-turn registry (native + this soul's MCP
// tools).
//
// Returns nil — meaning "no filtering, every registered tool is available" —
// for a nil soul (framework consumers outside the vaelum model) and on any
// DB error (a config-store blip must never strand the soul).
func (g *Gateway) allowedToolsForSoul(ctx context.Context, soulID uuid.UUID, registry *bs.ToolRegistry) []string {
	if soulID == uuid.Nil || registry == nil {
		return nil
	}
	db, err := g.deps.DB("ship")
	if err != nil {
		return nil
	}

	// Per-tool enable/disable overrides chosen in the cabinet.
	var toolRows []struct {
		ToolName string `db:"tool_name"`
		Enabled  bool   `db:"enabled"`
	}
	if err := db.SelectContext(ctx, &toolRows,
		`SELECT tool_name, enabled FROM vaelum.soul_tools WHERE soul_id = $1`,
		soulID); err != nil {
		g.logger.Warn("soul tools: query failed, allowing all tools",
			"soul_id", soulID.String(), "error", err)
		return nil
	}
	override := make(map[string]bool, len(toolRows))
	for _, r := range toolRows {
		override[r.ToolName] = r.Enabled
	}

	// Service providers this soul has connected (a probe has succeeded).
	var providers []string
	if err := db.SelectContext(ctx, &providers,
		`SELECT provider FROM vaelum.tool_credentials
		 WHERE soul_id = $1 AND status = 'connected'`,
		soulID); err != nil {
		g.logger.Warn("soul tools: provider query failed, allowing all tools",
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
