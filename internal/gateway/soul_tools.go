package gateway

import (
	"context"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/core"
)

// allowedToolsForSoul returns the set of tool names a soul may use: every
// core tool (internal machinery — always on) plus every non-core tool not
// explicitly disabled in vaelum.soul_tools. The result is handed to the
// agent loop as RunConfig.AllowedTools — a hard per-turn allowlist, so a
// cabinet toggle takes effect on the very next message. registry is the
// per-turn registry (native + this soul's MCP tools).
//
// Returns nil — meaning "no filtering, every registered tool is available" —
// for a soul with no soul_tools rows (the default, unchanged behaviour),
// for a nil soul (framework consumers outside the vaelum model), and on any
// DB error (a config-store blip must never strand the soul).
func (g *Gateway) allowedToolsForSoul(ctx context.Context, soulID uuid.UUID, registry *bs.ToolRegistry) []string {
	if soulID == uuid.Nil || registry == nil {
		return nil
	}
	db, err := g.deps.DB("ship")
	if err != nil {
		return nil
	}
	var rows []struct {
		ToolName string `db:"tool_name"`
		Enabled  bool   `db:"enabled"`
	}
	if err := db.SelectContext(ctx, &rows,
		`SELECT tool_name, enabled FROM vaelum.soul_tools WHERE soul_id = $1`,
		soulID); err != nil {
		g.logger.Warn("soul tools: query failed, allowing all tools",
			"soul_id", soulID.String(), "error", err)
		return nil
	}
	if len(rows) == 0 {
		return nil // soul never touched tool config — everything on
	}

	override := make(map[string]bool, len(rows))
	for _, r := range rows {
		override[r.ToolName] = r.Enabled
	}
	meta := g.deps.Config.ToolMeta

	var allowed []string
	for _, def := range registry.Definitions() {
		name := def.Name
		if meta[name].Core {
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
