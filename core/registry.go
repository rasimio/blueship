package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jmoiron/sqlx"
)

// ToolRegistry manages tool definitions and dispatches tool calls.
type ToolRegistry struct {
	tools map[string]registeredTool
}

// ToolMode classifies a tool by its expected execution profile. Duplicated
// as a raw string here so core does not have to import the a2a package.
type ToolMode = string

const (
	ToolModeSync  ToolMode = "sync"
	ToolModeAsync ToolMode = "async"
)

type registeredTool struct {
	Definition ToolDefinition
	Handler    ToolHandler

	// A2A metadata — only populated for tools that opt into exposure.
	Mode    ToolMode
	Exposed bool
	Remote  bool   // true if this is a RemoteTool wrapping an HTTP client
	PeerTag string // originating peer name for remote tools (audit/debug)
}

// NewToolRegistry creates a new empty tool registry.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]registeredTool)}
}

// Register adds a tool to the registry. Tools registered via this call
// default to Mode=sync and Exposed=false (local-only). Use Expose to
// make a tool visible on the A2A bus.
func (r *ToolRegistry) Register(name, description string, schema json.RawMessage, handler ToolHandler) {
	r.tools[name] = registeredTool{
		Definition: ToolDefinition{
			Name:        name,
			Description: description,
			InputSchema: schema,
		},
		Handler: handler,
		Mode:    ToolModeSync,
	}
}

// Expose marks a previously-registered tool as A2A-visible and sets its
// execution mode. The server layer discovers exposed tools via the new
// ExposedTools() accessor.
func (r *ToolRegistry) Expose(name string, mode ToolMode) bool {
	t, ok := r.tools[name]
	if !ok {
		return false
	}
	t.Exposed = true
	t.Mode = mode
	r.tools[name] = t
	return true
}

// RegisterRemote installs a proxy tool that wraps an HTTP call to a peer.
// The handler does the network dispatch; the registry only needs the
// metadata to expose the tool to the cortex alongside local ones.
func (r *ToolRegistry) RegisterRemote(name, description string, schema json.RawMessage, mode ToolMode, peerName string, handler ToolHandler) {
	r.tools[name] = registeredTool{
		Definition: ToolDefinition{
			Name:        name,
			Description: description,
			InputSchema: schema,
		},
		Handler: handler,
		Mode:    mode,
		Remote:  true,
		PeerTag: peerName,
	}
}

// ExposedTools returns {name, mode, description, schema} for every tool
// marked Exposed. Used by the a2a server to build /.well-known/agent.
type ExposedToolInfo struct {
	Name        string
	Description string
	Mode        ToolMode
	Schema      json.RawMessage
}

func (r *ToolRegistry) ExposedTools() []ExposedToolInfo {
	out := make([]ExposedToolInfo, 0)
	for _, t := range r.tools {
		if !t.Exposed {
			continue
		}
		out = append(out, ExposedToolInfo{
			Name:        t.Definition.Name,
			Description: t.Definition.Description,
			Mode:        t.Mode,
			Schema:      t.Definition.InputSchema,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ToolMetadata returns mode/exposed/remote flags for a registered tool.
func (r *ToolRegistry) ToolMetadata(name string) (mode ToolMode, exposed, remote bool, ok bool) {
	t, found := r.tools[name]
	if !found {
		return "", false, false, false
	}
	return t.Mode, t.Exposed, t.Remote, true
}

// HandlerByName returns the raw ToolHandler for the named tool. Used by
// the a2a server dispatcher so it can run an exposed tool directly.
func (r *ToolRegistry) HandlerByName(name string) (ToolHandler, bool) {
	t, ok := r.tools[name]
	if !ok {
		return nil, false
	}
	return t.Handler, true
}

// Definitions returns all registered tool definitions sorted by name.
func (r *ToolRegistry) Definitions() []ToolDefinition {
	defs := make([]ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.Definition)
	}
	sort.Slice(defs, func(i, j int) bool {
		return defs[i].Name < defs[j].Name
	})
	return defs
}

// DefinitionsForNames returns tool definitions for the given names, preserving order.
// Unknown names are silently skipped.
func (r *ToolRegistry) DefinitionsForNames(names []string) []ToolDefinition {
	defs := make([]ToolDefinition, 0, len(names))
	for _, name := range names {
		if t, ok := r.tools[name]; ok {
			defs = append(defs, t.Definition)
		}
	}
	return defs
}

// Execute runs a tool by name and returns the result JSON string and whether it's an error.
func (r *ToolRegistry) Execute(ctx context.Context, name string, input json.RawMessage) (string, bool) {
	tool, ok := r.tools[name]
	if !ok {
		return fmt.Sprintf("unknown tool: %s", name), true
	}

	result, err := tool.Handler(ctx, input)
	if err != nil {
		return err.Error(), true
	}

	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("marshal result: %s", err), true
	}

	return string(data), false
}

// SubsetForNames creates a new ToolRegistry containing only the named tools.
// Both definitions and handlers are copied, so the subset is fully executable.
func (r *ToolRegistry) SubsetForNames(names []string) *ToolRegistry {
	sub := NewToolRegistry()
	for _, name := range names {
		if t, ok := r.tools[name]; ok {
			sub.tools[name] = t
		}
	}
	return sub
}

// Count returns the number of registered tools.
func (r *ToolRegistry) Count() int {
	return len(r.tools)
}

// LoadDescriptions loads tool descriptions from the tool_descriptions DB table.
// Every registered tool MUST have a row in the table. Missing = error at startup.
func (r *ToolRegistry) LoadDescriptions(db *sqlx.DB) error {
	var rows []struct {
		Name string `db:"name"`
		Desc string `db:"description"`
	}
	if err := db.Select(&rows, `SELECT name, description FROM tool_descriptions`); err != nil {
		return fmt.Errorf("load tool_descriptions: %w", err)
	}

	descs := make(map[string]string, len(rows))
	for _, row := range rows {
		descs[row.Name] = row.Desc
	}

	var missing []string
	for name, tool := range r.tools {
		if desc, ok := descs[name]; ok {
			tool.Definition.Description = desc
			r.tools[name] = tool
		} else {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("tool_descriptions missing for: %v", missing)
	}

	return nil
}
