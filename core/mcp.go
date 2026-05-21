package core

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// MCPTool is a tool discovered from an external MCP server, ready to be
// registered into a soul's ToolRegistry as a remote tool.
type MCPTool struct {
	Name        string // namespaced registry name, e.g. mcp__notion__search
	Description string
	Schema      json.RawMessage
	Handler     ToolHandler
}

// MCPToolSource supplies a soul's external MCP tools to the gateway. The
// host (arlene) provides the implementation; blueship core only declares
// the seam so core need not import the mcp package.
type MCPToolSource interface {
	// ToolsForSoul returns every tool from the soul's connected MCP
	// servers. It never returns an error — a soul whose servers are all
	// down or unconfigured simply yields an empty slice.
	ToolsForSoul(ctx context.Context, soulID uuid.UUID) []MCPTool
	// Invalidate drops cached connections for a soul so the next
	// ToolsForSoul reconnects — called when the cabinet changes config.
	Invalidate(soulID uuid.UUID)
}
