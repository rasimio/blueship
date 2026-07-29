package core

import (
	"context"
	"encoding/json"
	"testing"
)

// handlerReturning builds a tool handler that reports which registration
// answered the call, so a test can tell local from peer dispatch.
func handlerReturning(id string) ToolHandler {
	return func(context.Context, json.RawMessage) (any, error) { return id, nil }
}

func callTool(t *testing.T, h ToolHandler) string {
	t.Helper()
	out, err := h(context.Background(), nil)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	s, ok := out.(string)
	if !ok {
		t.Fatalf("handler returned %T, want string", out)
	}
	return s
}

// A peer exposing a name this ship implements locally must not capture
// that name. This is the agent_task_status regression: the peer's copy
// took the slot, so reading our own task state hit a ship that had never
// stored it and failed with "no rows in result set".
func TestRegisterRemoteDoesNotClobberLocalTool(t *testing.T) {
	r := NewToolRegistry()
	r.Register("agent_task_status", "local", nil, handlerReturning("local"))
	r.RegisterRemote("agent_task_status", "peer", nil, ToolModeSync, "liya", handlerReturning("peer"))

	h, ok := r.HandlerByName("agent_task_status")
	if !ok {
		t.Fatal("agent_task_status missing from registry")
	}
	if got := callTool(t, h); got != "local" {
		t.Errorf("bare name resolved to %q, want the local registration", got)
	}
	if _, _, remote, _ := r.ToolMetadata("agent_task_status"); remote {
		t.Error("bare name is flagged remote; local registration should own it")
	}
}

// Registration order must not decide the winner: Fleet bootstrap can push
// a snapshot before or after local modules register.
func TestLocalToolWinsRegardlessOfOrder(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterRemote("agent_task_status", "peer", nil, ToolModeSync, "liya", handlerReturning("peer"))
	r.Register("agent_task_status", "local", nil, handlerReturning("local"))

	h, _ := r.HandlerByName("agent_task_status")
	if got := callTool(t, h); got != "local" {
		t.Errorf("remote-then-local resolved to %q, want local", got)
	}
}

// Shadowing a name must not make the peer unreachable — delegation still
// has to be able to submit to and poll the peer.
func TestRemoteHandlerForPeerReachesShadowedTool(t *testing.T) {
	r := NewToolRegistry()
	r.Register("agent_task_accept", "local", nil, handlerReturning("local"))
	r.RegisterRemote("agent_task_accept", "peer", nil, ToolModeSync, "liya", handlerReturning("peer"))

	h, ok := r.RemoteHandlerForPeer("liya", "agent_task_accept")
	if !ok {
		t.Fatal("peer copy unreachable after local registration took the name")
	}
	if got := callTool(t, h); got != "peer" {
		t.Errorf("RemoteHandlerForPeer dispatched to %q, want peer", got)
	}
}

// delegate_to carries a peer agent id while Fleet tags registrations with
// the agent name, so an exact miss falls back to the only remote copy.
func TestRemoteHandlerForPeerFallsBackWhenUnambiguous(t *testing.T) {
	r := NewToolRegistry()
	r.Register("agent_task_accept", "local", nil, handlerReturning("local"))
	r.RegisterRemote("agent_task_accept", "peer", nil, ToolModeSync, "liya", handlerReturning("peer"))

	h, ok := r.RemoteHandlerForPeer("2f9c-agent-id-not-the-name", "agent_task_accept")
	if !ok {
		t.Fatal("unambiguous fallback failed; delegation by agent id would break")
	}
	if got := callTool(t, h); got != "peer" {
		t.Errorf("fallback dispatched to %q, want peer", got)
	}
}

// With several peers offering the name and no exact match, refuse rather
// than hand the task to an arbitrary ship.
func TestRemoteHandlerForPeerRefusesAmbiguousFallback(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterRemote("agent_task_accept", "peer", nil, ToolModeSync, "liya", handlerReturning("liya"))
	r.RegisterRemote("agent_task_accept", "peer", nil, ToolModeSync, "kate", handlerReturning("kate"))

	if _, ok := r.RemoteHandlerForPeer("unknown-id", "agent_task_accept"); ok {
		t.Error("ambiguous fallback resolved; want refusal")
	}
	// An exact peer match must still work when the fleet has many peers.
	h, ok := r.RemoteHandlerForPeer("kate", "agent_task_accept")
	if !ok {
		t.Fatal("exact peer match failed in a multi-peer fleet")
	}
	if got := callTool(t, h); got != "kate" {
		t.Errorf("exact match dispatched to %q, want kate", got)
	}
}

// A peer tool with no local counterpart is the common case and must still
// be callable by its bare name from the cortex.
func TestRemoteOnlyToolKeepsBareName(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterRemote("code_repo_sync", "peer", nil, ToolModeSync, "liya", handlerReturning("peer"))

	h, ok := r.HandlerByName("code_repo_sync")
	if !ok {
		t.Fatal("remote-only tool missing from registry")
	}
	if got := callTool(t, h); got != "peer" {
		t.Errorf("remote-only tool resolved to %q, want peer", got)
	}
}

// A later Fleet snapshot must replace the previous remote handler for the
// same peer rather than accumulate stale endpoints.
func TestRegisterRemoteReplacesSamePeerRegistration(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterRemote("code_repo_sync", "peer", nil, ToolModeSync, "liya", handlerReturning("stale"))
	r.RegisterRemote("code_repo_sync", "peer", nil, ToolModeSync, "liya", handlerReturning("fresh"))

	h, _ := r.RemoteHandlerForPeer("liya", "code_repo_sync")
	if got := callTool(t, h); got != "fresh" {
		t.Errorf("peer lookup returned %q, want the refreshed handler", got)
	}
}

// Clone and SubsetForNames build the registries the gateway and scheduler
// actually dispatch through; peer reachability has to survive both.
func TestClonePreservesPeerAddressableTools(t *testing.T) {
	r := NewToolRegistry()
	r.Register("agent_task_accept", "local", nil, handlerReturning("local"))
	r.RegisterRemote("agent_task_accept", "peer", nil, ToolModeSync, "liya", handlerReturning("peer"))

	h, ok := r.Clone().RemoteHandlerForPeer("liya", "agent_task_accept")
	if !ok {
		t.Fatal("clone lost the peer copy; delegation would fail per turn")
	}
	if got := callTool(t, h); got != "peer" {
		t.Errorf("clone dispatched to %q, want peer", got)
	}
}

func TestSubsetPreservesPeerAddressableTools(t *testing.T) {
	r := NewToolRegistry()
	r.Register("agent_task_accept", "local", nil, handlerReturning("local"))
	r.RegisterRemote("agent_task_accept", "peer", nil, ToolModeSync, "liya", handlerReturning("peer"))

	sub := r.SubsetForNames([]string{"agent_task_accept"})
	h, ok := sub.RemoteHandlerForPeer("liya", "agent_task_accept")
	if !ok {
		t.Fatal("subset lost the peer copy")
	}
	if got := callTool(t, h); got != "peer" {
		t.Errorf("subset dispatched to %q, want peer", got)
	}
	// The cortex-facing name in the subset stays local.
	lh, _ := sub.HandlerByName("agent_task_accept")
	if got := callTool(t, lh); got != "local" {
		t.Errorf("subset bare name resolved to %q, want local", got)
	}
}
