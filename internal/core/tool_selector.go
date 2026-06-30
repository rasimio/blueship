package core

import "context"

// ToolSelectionRequest is a host-provided per-turn tool arbitration input.
// BlueShip stays generic: the host owns domain-specific packs and routing
// rules, while the gateway supplies the current role, message, and already
// matched rule/reflex tool hints.
type ToolSelectionRequest struct {
	Role            string
	UserText        string
	BaseTools       []string
	ForcedTools     []string
	ReflexGuidance  string
	InjectedContext string
	Ephemeral       bool
}

// ToolSelection is the host's explicit per-turn tool list.
//
// Tools == nil means "do not override the role default". Tools == [] means
// "send no tools this turn". Source is diagnostic text for logs/traces.
type ToolSelection struct {
	Tools  []string
	Source string
}

// ToolSelector chooses the concrete tool subset sent to the LLM for one turn.
type ToolSelector func(ctx context.Context, req ToolSelectionRequest) ToolSelection
