package core

import (
	"context"
	"encoding/json"
	"fmt"
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

// Definitions returns all registered tool definitions for the LLM API request.
func (r *ToolRegistry) Definitions() []ToolDefinition {
	defs := make([]ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.Definition)
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
