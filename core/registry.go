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
	Priority   int // lower = first in the list (primacy bias for small models)
}

// NewToolRegistry creates a new empty tool registry.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]registeredTool)}
}

// Register adds a tool to the registry with default priority (100).
func (r *ToolRegistry) Register(name, description string, schema json.RawMessage, handler ToolHandler) {
	r.tools[name] = registeredTool{
		Definition: ToolDefinition{
			Name:        name,
			Description: description,
			InputSchema: schema,
		},
		Handler:  handler,
		Priority: 100,
	}
}

// RegisterWithPriority adds a tool with explicit priority (lower = listed first).
func (r *ToolRegistry) RegisterWithPriority(name, description string, schema json.RawMessage, handler ToolHandler, priority int) {
	r.tools[name] = registeredTool{
		Definition: ToolDefinition{
			Name:        name,
			Description: description,
			InputSchema: schema,
		},
		Handler:  handler,
		Priority: priority,
	}
}

// Definitions returns all registered tool definitions sorted by priority (low first).
// Primacy bias: small models are more likely to use tools that appear early.
func (r *ToolRegistry) Definitions() []ToolDefinition {
	type entry struct {
		def      ToolDefinition
		priority int
	}
	entries := make([]entry, 0, len(r.tools))
	for _, t := range r.tools {
		entries = append(entries, entry{def: t.Definition, priority: t.Priority})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].priority != entries[j].priority {
			return entries[i].priority < entries[j].priority
		}
		return entries[i].def.Name < entries[j].def.Name
	})
	defs := make([]ToolDefinition, len(entries))
	for i, e := range entries {
		defs[i] = e.def
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

// Count returns the number of registered tools.
func (r *ToolRegistry) Count() int {
	return len(r.tools)
}
