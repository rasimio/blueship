package core

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ExecutionKind tells a host whether the requested work was initiated by a
// person or by the background scheduler. It deliberately says nothing about
// product policy: those decisions belong to the embedding app.
type ExecutionKind string

const (
	ExecutionInteractive ExecutionKind = "interactive"
	ExecutionBackground  ExecutionKind = "background"
)

// ExecutionRequest is the generic admission context exposed to a host.
type ExecutionRequest struct {
	UserID    uuid.UUID
	SoulID    uuid.UUID
	Kind      ExecutionKind
	Transport string
}

// ExecutionDecision is the host's admission result. Reason is for logs and
// must not be shown directly to end users.
type ExecutionDecision struct {
	Allowed bool
	Reason  string
}

// ExecutionAuthorizer is an optional host policy hook. Nil means allow, which
// preserves BlueShip's behavior for standalone and single-user deployments.
type ExecutionAuthorizer func(context.Context, ExecutionRequest) (ExecutionDecision, error)

// ErrExecutionDenied lets non-Telegram callers distinguish policy denial from
// an infrastructure failure without importing any host-specific concept.
var ErrExecutionDenied = errors.New("blueship: execution denied")
