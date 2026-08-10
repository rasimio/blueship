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

// ExecutionDecision is the host's admission result.
type ExecutionDecision struct {
	Allowed bool

	// Reason is a stable code for logs and must not be shown to end users.
	Reason string

	// Actions are buttons offered under Message. Each carries the name of
	// a host command, so a refusal can hand somebody the way out instead
	// of describing it and hoping they type it.
	//
	// They exist because the alternative is a sentence like "send /plus
	// with your email" — which asks a person who has just been told no to
	// remember a command and compose an argument. Most will not.
	Actions []DecisionAction

	// Message is what the user reads when a turn is refused. Optional:
	// empty falls back to UIStrings.ExecutionDenied.
	//
	// It exists because a host's reasons for refusing are not
	// interchangeable — "your subscription lapsed" and "you have used
	// today's messages" need different words and lead somewhere
	// different — and only the host knows which applies, in what
	// language, with what numbers in it. The framework carries the
	// sentence; it never writes one.
	Message string
}

// DecisionAction is one button under a refusal. Command is the host
// command it invokes, dispatched exactly as if the person had typed it.
type DecisionAction struct {
	Label   string
	Command string
}

// ExecutionAuthorizer is an optional host policy hook. Nil means allow, which
// preserves BlueShip's behavior for standalone and single-user deployments.
type ExecutionAuthorizer func(context.Context, ExecutionRequest) (ExecutionDecision, error)

// ErrExecutionDenied lets non-Telegram callers distinguish policy denial from
// an infrastructure failure without importing any host-specific concept.
var ErrExecutionDenied = errors.New("blueship: execution denied")
