package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// ToolRegistry manages tool definitions and dispatches tool calls.
type ToolRegistry struct {
	tools map[string]registeredTool
}

type registeredTool struct {
	Definition ToolDefinition
	Handler    ToolHandler
}

// NewToolRegistry creates a new empty tool registry.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]registeredTool)}
}

// Register adds a tool to the registry.
func (r *ToolRegistry) Register(name, description string, schema json.RawMessage, handler ToolHandler) {
	r.tools[name] = registeredTool{
		Definition: ToolDefinition{
			Name:        name,
			Description: description,
			InputSchema: schema,
		},
		Handler: handler,
	}
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
