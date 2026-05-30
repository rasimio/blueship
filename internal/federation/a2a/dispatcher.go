package a2a

import (
	"context"
	"encoding/json"
	"fmt"
)

// DispatcherBackend is the minimal interface a2a/server needs to run
// exposed tools. blueship provides RegistryDispatcher (below) as the
// default implementation — it proxies tool execution through the core
// ToolRegistry, keeping exposed tools reusable from both cortex and A2A.
type DispatcherBackend interface {
	Tool(name string) (ExposedTool, bool)
	InvokeSync(ctx context.Context, name string, input json.RawMessage) (json.RawMessage, error)
	InvokeAsync(ctx context.Context, name string, input json.RawMessage, emit EventEmitter) (json.RawMessage, error)
}

// RegistryLike is the subset of core.ToolRegistry that the dispatcher
// needs. Declared here so blueship/a2a does not import blueship/core
// (which would create a cycle when core uses a2a constants).
type RegistryLike interface {
	ExposedTools() []ExposedToolInfoLike
	ToolMetadata(name string) (mode string, exposed, remote bool, ok bool)
	HandlerByName(name string) (func(ctx context.Context, input json.RawMessage) (any, error), bool)
}

// ExposedToolInfoLike mirrors core.ExposedToolInfo — duplicated to avoid
// the circular import. Shipping code converts from the core flavour to
// this one in one place (see blueship/a2a_adapter.go in the ship layer).
type ExposedToolInfoLike struct {
	Name        string
	Description string
	Mode        string
	Schema      json.RawMessage
}

// RegistryDispatcher adapts a core.ToolRegistry (via RegistryLike) to
// the server.Dispatcher interface. Async tools currently run as inline
// sync handlers — the server still treats them as async from the caller's
// point of view (it returns a handle and streams events), but the actual
// work happens within InvokeAsync's single call. Real streaming emitters
// are added in the second integration pass when domain modules produce
// their own events.
type RegistryDispatcher struct {
	reg RegistryLike
}

// NewRegistryDispatcher constructs a dispatcher.
func NewRegistryDispatcher(reg RegistryLike) *RegistryDispatcher {
	return &RegistryDispatcher{reg: reg}
}

// ExposedTools returns all exposed tools with schemas from the registry.
func (d *RegistryDispatcher) ExposedTools() []ExposedTool {
	src := d.reg.ExposedTools()
	out := make([]ExposedTool, 0, len(src))
	for _, t := range src {
		out = append(out, ExposedTool{
			Name:        t.Name,
			Description: t.Description,
			Mode:        ToolMode(t.Mode),
			Schema:      t.Schema,
		})
	}
	return out
}

// Tool returns metadata for the named exposed tool.
func (d *RegistryDispatcher) Tool(name string) (ExposedTool, bool) {
	mode, exposed, _, ok := d.reg.ToolMetadata(name)
	if !ok || !exposed {
		return ExposedTool{}, false
	}
	for _, t := range d.reg.ExposedTools() {
		if t.Name != name {
			continue
		}
		return ExposedTool{
			Name:        t.Name,
			Description: t.Description,
			Mode:        ToolMode(mode),
			Schema:      t.Schema,
		}, true
	}
	return ExposedTool{}, false
}

// InvokeSync runs a tool synchronously and marshals its result to JSON.
func (d *RegistryDispatcher) InvokeSync(ctx context.Context, name string, input json.RawMessage) (json.RawMessage, error) {
	handler, ok := d.reg.HandlerByName(name)
	if !ok {
		return nil, fmt.Errorf("tool %q not found", name)
	}
	result, err := handler(ctx, input)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return json.RawMessage("null"), nil
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal tool result: %w", err)
	}
	return data, nil
}

// InvokeAsync runs an async tool. MVP implementation: runs as sync, emits
// a single terminal event with the result, and returns the same value as
// the initial payload. Real async streaming per tool arrives in Phase 2.
func (d *RegistryDispatcher) InvokeAsync(ctx context.Context, name string, input json.RawMessage, emit EventEmitter) (json.RawMessage, error) {
	out, err := d.InvokeSync(ctx, name, input)
	if err != nil {
		if emit != nil {
			_ = emit.EmitTerminal(ctx, CallStateFailed, marshalErrJSON(err))
		}
		return nil, err
	}
	if emit != nil {
		_ = emit.EmitTerminal(ctx, CallStateDone, out)
	}
	return out, nil
}

func marshalErrJSON(err error) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	return b
}
