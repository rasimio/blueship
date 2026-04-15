// Package a2a is blueship's Agent-to-Agent protocol — a universal tool bus
// that lets any ship expose selected local tools to other ships over HTTP
// and import interesting tools from its peers. The cortex calls remote tools
// the same way it calls local ones; the a2a client transparently dispatches
// cross-ship calls via HTTP while tracing every interaction to a Telegram
// group chat so owners can follow inter-agent conversations on the phone.
package a2a

import (
	"context"
	"encoding/json"
	"time"
)

// ToolMode classifies a tool by its expected execution profile.
type ToolMode string

const (
	// ToolModeSync is a fast blocking call (target < 30s). The HTTP invoke
	// endpoint returns the result inline.
	ToolModeSync ToolMode = "sync"

	// ToolModeAsync covers long-running work that must stream events back
	// to the caller. /a2a/invoke returns a handle immediately and the
	// caller subscribes to /a2a/events via SSE.
	ToolModeAsync ToolMode = "async"
)

// CallDirection records whether a call was made FROM this ship or TO it.
type CallDirection string

const (
	CallDirectionOut CallDirection = "out" // this ship invoked a peer's tool
	CallDirectionIn  CallDirection = "in"  // a peer invoked a local exposed tool
)

// CallState is the lifecycle state of a single a2a invocation.
type CallState string

const (
	CallStatePending  CallState = "pending"
	CallStateRunning  CallState = "running"
	CallStateDone     CallState = "done"
	CallStateFailed   CallState = "failed"
	CallStateCanceled CallState = "canceled"
)

// IsTerminal reports whether the call has reached a final state.
func (s CallState) IsTerminal() bool {
	switch s {
	case CallStateDone, CallStateFailed, CallStateCanceled:
		return true
	}
	return false
}

// EventType classifies a streaming event on an async call.
type EventType string

const (
	EventTypeStateChange EventType = "state_change"
	EventTypeOutput      EventType = "output"
	EventTypeLog         EventType = "log"
	EventTypeTerminal    EventType = "terminal"
)

// ExposedTool is a locally-registered tool that has been marked for A2A
// exposure. It is discoverable via /.well-known/agent and invokable via
// /a2a/invoke. The Handler is the same ToolHandler already registered in
// the ToolRegistry — a2a only adds the metadata layer.
type ExposedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Mode        ToolMode        `json:"mode"`
	Schema      json.RawMessage `json:"schema"`
}

// RemoteTool is the projection of a tool exposed by a peer that this ship
// has imported. At startup the client reads agent cards from configured
// peers, caches their RemoteTool entries, and registers each one in the
// local ToolRegistry as a thin RemoteTool handler that wraps an HTTP call.
type RemoteTool struct {
	PeerName    string          `json:"peer_name"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Mode        ToolMode        `json:"mode"`
	Schema      json.RawMessage `json:"schema"`
	LastSeenAt  time.Time       `json:"last_seen_at"`
}

// AgentCard is the body of /.well-known/agent. It describes the ship to
// potential callers: name, endpoints, auth requirements, and the full list
// of tools currently exposed for A2A invocation.
type AgentCard struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Version     string        `json:"version"`
	BaseURL     string        `json:"base_url"`
	Endpoints   Endpoints     `json:"endpoints"`
	Auth        AuthInfo      `json:"auth"`
	Tools       []ExposedTool `json:"tools"`
}

// Endpoints lists the A2A entry points a client needs to know.
type Endpoints struct {
	Invoke string `json:"invoke"` // "/a2a/invoke"
	Events string `json:"events"` // "/a2a/events"
	Cancel string `json:"cancel"` // "/a2a/cancel"
}

// AuthInfo describes the authentication scheme the ship expects.
type AuthInfo struct {
	Type string `json:"type"` // "bearer" for shared secret
}

// Peer is a remote ship this one knows about.
type Peer struct {
	ID            string    `db:"id"`
	Name          string    `db:"name"`
	BaseURL       string    `db:"base_url"`
	AuthToken     string    `db:"auth_token"`
	AgentCard     []byte    `db:"agent_card"`
	CardFetchedAt *time.Time `db:"card_fetched_at"`
	LastSeenAt    *time.Time `db:"last_seen_at"`
	Enabled       bool      `db:"enabled"`
	CreatedAt     time.Time `db:"created_at"`
	UpdatedAt     time.Time `db:"updated_at"`
}

// Call is one row in the a2a_calls audit table.
type Call struct {
	ID            string          `db:"id"`
	PeerID        *string         `db:"peer_id"`
	PeerName      string          `db:"peer_name"`
	Direction     CallDirection   `db:"direction"`
	ToolName      string          `db:"tool_name"`
	Mode          ToolMode        `db:"mode"`
	CorrelationID *string         `db:"correlation_id"`
	Input         json.RawMessage `db:"input"`
	Output        json.RawMessage `db:"output"`
	Error         *string         `db:"error"`
	State         CallState       `db:"state"`
	CreatedAt     time.Time       `db:"created_at"`
	CompletedAt   *time.Time      `db:"completed_at"`
}

// Event is one streamed update on an async call. Clients reading via SSE
// receive these in order; when IsFinal is true the stream terminates.
type Event struct {
	ID        int64           `db:"id"`
	CallID    string          `db:"call_id"`
	Seq       int             `db:"seq"`
	Type      EventType       `db:"type"`
	Payload   json.RawMessage `db:"payload"`
	IsFinal   bool            `db:"is_final"`
	CreatedAt time.Time       `db:"created_at"`
}

// InvokeRequest is the body of POST /a2a/invoke.
type InvokeRequest struct {
	Tool          string          `json:"tool"`
	Input         json.RawMessage `json:"input"`
	CorrelationID string          `json:"correlation_id,omitempty"`
}

// InvokeResponse is the body returned from POST /a2a/invoke. For sync
// tools Output is populated; for async tools Handle is populated and the
// caller must subscribe to EventsURL via SSE.
type InvokeResponse struct {
	CallID    string          `json:"call_id"`
	Mode      ToolMode        `json:"mode"`
	State     CallState       `json:"state"`
	Output    json.RawMessage `json:"output,omitempty"`
	Handle    string          `json:"handle,omitempty"`
	EventsURL string          `json:"events_url,omitempty"`
}

// APIError is the standard error body for HTTP 4xx/5xx responses.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// Error implements the error interface so APIError can be returned from
// client methods.
func (e APIError) Error() string {
	if e.Details != "" {
		return e.Code + ": " + e.Message + " (" + e.Details + ")"
	}
	return e.Code + ": " + e.Message
}

// ToolHandler is the signature every A2A-exposed tool must satisfy.
// Implementations live in the owning domain module; a2a only wraps and
// dispatches. The handler returns opaque output (marshalled back to JSON)
// or a typed APIError for negative responses.
type ToolHandler func(ctx context.Context, input json.RawMessage) (output json.RawMessage, err error)

// EventEmitter is handed to async tool handlers so they can push progress
// events while the long-running work continues. Emit calls MUST be in
// sequence; the receiver numbers them automatically. Terminal events (done,
// failed, canceled) are emitted via EmitTerminal which closes the stream
// and transitions the call state.
type EventEmitter interface {
	Emit(ctx context.Context, eventType EventType, payload json.RawMessage) error
	EmitTerminal(ctx context.Context, finalState CallState, payload json.RawMessage) error
}

// AsyncToolHandler is the signature for async tools. The handler returns
// immediately with the initial output (can be empty) and continues work in
// a background goroutine, emitting events via the EventEmitter until it
// calls EmitTerminal.
type AsyncToolHandler func(ctx context.Context, input json.RawMessage, emit EventEmitter) (initial json.RawMessage, err error)
