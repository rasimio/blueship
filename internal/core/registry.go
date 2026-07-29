package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rasimio/blueship/telemetry"
)

// peerPrefix is the sentinel that triggers peer-wildcard expansion in
// DefinitionsForNames / SubsetForNames. "peer:<name>" means "all tools
// whose PeerTag == <name>, sorted alphabetically."
const peerPrefix = "peer:"

// toolPeerAttr is a tiny helper kept here so registry.go doesn't import
// otel/attribute at multiple call sites. Keeps the SetAttributes line
// readable when remote-tool peer tagging is added.
func toolPeerAttr(peer string) attribute.KeyValue {
	return attribute.String("tool.peer", peer)
}

// ToolRegistry manages tool definitions and dispatches tool calls.
type ToolRegistry struct {
	tools map[string]registeredTool
	// remoteByPeer holds every remote registration keyed by peer+name,
	// including ones that lost the bare-name slot to a local tool. It is
	// what lets delegation address a peer's copy of a name this ship also
	// implements locally (agent_task_accept / agent_task_status).
	remoteByPeer map[string]registeredTool
}

// remoteKey builds the composite key for remoteByPeer. The separator is
// a NUL so it cannot collide with a peer or tool name.
func remoteKey(peer, name string) string { return peer + "\x00" + name }

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
	return &ToolRegistry{
		tools:        make(map[string]registeredTool),
		remoteByPeer: make(map[string]registeredTool),
	}
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
//
// A local registration owns its name: when this ship already implements
// `name` itself, the peer's copy is kept in remoteByPeer but does not take
// the bare-name slot. Without that rule a peer exposing a name we also
// implement silently hijacks every local call — `agent_task_status` used to
// resolve to a peer that had never heard of this ship's tasks, so reading
// our own task state failed with "no rows in result set". Callers that
// genuinely want the peer's copy ask for it by peer via
// RemoteHandlerForPeer.
func (r *ToolRegistry) RegisterRemote(name, description string, schema json.RawMessage, mode ToolMode, peerName string, handler ToolHandler) {
	rt := registeredTool{
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
	if r.remoteByPeer == nil {
		r.remoteByPeer = make(map[string]registeredTool)
	}
	r.remoteByPeer[remoteKey(peerName, name)] = rt
	if existing, ok := r.tools[name]; ok && !existing.Remote {
		return
	}
	r.tools[name] = rt
}

// RemoteHandlerForPeer returns the handler for a peer's copy of a tool,
// even when a local registration owns the bare name. Delegation uses this
// to reach the peer explicitly instead of relying on name shadowing.
//
// peer is matched against the PeerTag recorded at registration, which Fleet
// sets to the peer's agent *name*. A task's delegate_to may instead hold the
// peer's agent *id*, so an exact miss falls back to the sole remote
// registration of that name — unambiguous in a one-peer fleet and identical
// to the pre-existing shadowing behaviour. An ambiguous fallback (several
// peers exposing the name) resolves to none, since guessing which ship to
// hand a task to is worse than failing loudly.
func (r *ToolRegistry) RemoteHandlerForPeer(peer, name string) (ToolHandler, bool) {
	if t, ok := r.remoteByPeer[remoteKey(peer, name)]; ok {
		return t.Handler, true
	}
	var found *registeredTool
	for key, t := range r.remoteByPeer {
		if strings.HasSuffix(key, "\x00"+name) {
			if found != nil {
				return nil, false
			}
			candidate := t
			found = &candidate
		}
	}
	if found == nil {
		return nil, false
	}
	return found.Handler, true
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

// Has reports whether an exact tool name is registered. It deliberately does
// not expand peer wildcards: typed task programs execute concrete tools, and
// must fail explicitly when a configured integration is unavailable.
func (r *ToolRegistry) Has(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.tools[name]
	return ok
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

// PeerForTool returns the peer name for a remote tool, or "" for local tools.
func (r *ToolRegistry) PeerForTool(name string) string {
	if t, ok := r.tools[name]; ok {
		return t.PeerTag
	}
	return ""
}

// toolsForPeer returns all registered tools whose PeerTag equals peer,
// sorted alphabetically by name for a stable iteration order.
func (r *ToolRegistry) toolsForPeer(peer string) []registeredTool {
	var out []registeredTool
	for _, t := range r.tools {
		if t.PeerTag == peer {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Definition.Name < out[j].Definition.Name
	})
	return out
}

// DefinitionsForNames returns tool definitions for the given names, preserving order.
// Unknown names are silently skipped. Duplicate names are deduplicated — first
// occurrence wins.
//
// Names with the prefix "peer:" expand to every tool registered from that peer
// (sorted alphabetically). Example: "peer:<name>" includes all tools BlueFleet
// received from that peer agent without requiring them to be listed individually.
// Expansion respects the seen-set, so an explicit name before "peer:xxx" takes
// precedence and the peer-expanded copy is skipped.
func (r *ToolRegistry) DefinitionsForNames(names []string) []ToolDefinition {
	seen := make(map[string]bool, len(names))
	defs := make([]ToolDefinition, 0, len(names))
	for _, name := range names {
		if strings.HasPrefix(name, peerPrefix) {
			peer := name[len(peerPrefix):]
			for _, t := range r.toolsForPeer(peer) {
				if !seen[t.Definition.Name] {
					seen[t.Definition.Name] = true
					defs = append(defs, t.Definition)
				}
			}
			continue
		}
		if !seen[name] {
			seen[name] = true
			if t, ok := r.tools[name]; ok {
				defs = append(defs, t.Definition)
			}
		}
	}
	return defs
}

// Execute runs a tool by name and returns the result JSON string and whether it's an error.
func (r *ToolRegistry) Execute(ctx context.Context, name string, input json.RawMessage) (string, bool) {
	if IsToolDenied(ctx, name) {
		return fmt.Sprintf("tool denied for this turn: %s", name), true
	}
	tool, ok := r.tools[name]
	if !ok {
		return fmt.Sprintf("unknown tool: %s", name), true
	}

	ctx, span := telemetry.StartToolSpan(ctx, name, len(input))
	defer span.End()
	if tool.Remote {
		span.SetAttributes(
			toolPeerAttr(tool.PeerTag),
		)
	}

	result, err := tool.Handler(ctx, input)
	if err != nil {
		out := err.Error()
		telemetry.AnnotateToolResult(span, len(out), true)
		telemetry.RecordError(span, err)
		return out, true
	}

	data, err := json.Marshal(result)
	if err != nil {
		out := fmt.Sprintf("marshal result: %s", err)
		telemetry.AnnotateToolResult(span, len(out), true)
		telemetry.RecordError(span, err)
		return out, true
	}

	telemetry.AnnotateToolResult(span, len(data), false)
	return string(data), false
}

// SubsetForNames creates a new ToolRegistry containing only the named tools.
// Both definitions and handlers are copied, so the subset is fully executable.
// Supports the same "peer:xxx" wildcard syntax as DefinitionsForNames.
func (r *ToolRegistry) SubsetForNames(names []string) *ToolRegistry {
	seen := make(map[string]bool, len(names))
	sub := NewToolRegistry()
	for _, name := range names {
		if strings.HasPrefix(name, peerPrefix) {
			peer := name[len(peerPrefix):]
			for _, t := range r.toolsForPeer(peer) {
				if !seen[t.Definition.Name] {
					seen[t.Definition.Name] = true
					sub.tools[t.Definition.Name] = t
				}
			}
			continue
		}
		if !seen[name] {
			seen[name] = true
			if t, ok := r.tools[name]; ok {
				sub.tools[name] = t
			}
		}
	}
	// The subset narrows what the cortex may call, not who the delegate
	// executor may reach; keep peer-addressable copies intact.
	for key, t := range r.remoteByPeer {
		sub.remoteByPeer[key] = t
	}
	return sub
}

// Count returns the number of registered tools.
func (r *ToolRegistry) Count() int {
	return len(r.tools)
}

// Clone returns a new registry holding the same tool entries. Used to
// compose a per-turn registry (native tools + a soul's MCP tools) without
// mutating the cached per-user registry.
func (r *ToolRegistry) Clone() *ToolRegistry {
	c := NewToolRegistry()
	for name, t := range r.tools {
		c.tools[name] = t
	}
	// Carry peer-addressable copies too, so a per-turn clone can still
	// dispatch delegation to a peer whose tool a local name shadows.
	for key, t := range r.remoteByPeer {
		c.remoteByPeer[key] = t
	}
	return c
}

// (Tool descriptions, modes, and exposed flags are declared inline at
// registration time. The earlier LoadToolsConfig/LoadDescriptions
// helpers, which read these from a `tools` DB table, are gone — code is
// the source of truth now. Use Register / Expose at the call site to
// fully describe each tool.)
