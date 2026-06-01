package core

import (
	"context"

	"github.com/google/uuid"
)

// chatIDCtxKey is a typed context key carrying the user's transport
// chat id (e.g. "telegram:5452235517") through the agent loop to tool
// handlers, so a tool can identify the originating chat without taking
// a snapshot of Deps. Set by the gateway before dispatching cortex.
type chatIDCtxKey struct{}

// ContextWithChatID returns a copy of ctx that carries the given chat
// id. Empty chat id is a no-op.
func ContextWithChatID(ctx context.Context, chatID string) context.Context {
	if chatID == "" {
		return ctx
	}
	return context.WithValue(ctx, chatIDCtxKey{}, chatID)
}

// ChatIDFromContext returns the chat id stashed via ContextWithChatID,
// or "" when the context wasn't tagged.
func ChatIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(chatIDCtxKey{}).(string)
	return v
}

// soulIDCtxKey is a typed context key carrying the tenant identity of
// the request — which soul this incoming message / agent task / CLI
// invocation belongs to. Resolved at transport boundaries (gateway,
// scheduler, CLI startup), threaded through ctx so every downstream
// repo INSERT can read it without needing soul-specific Deps wiring.
// One host runtime hosts N souls concurrently; soul is per-request,
// not per-process.
type soulIDCtxKey struct{}

// WithSoulID returns a copy of ctx carrying the given soul identity.
// Passing uuid.Nil is a no-op (treated as "no soul resolution
// happened yet"). Idempotent: re-setting overwrites cleanly.
func WithSoulID(ctx context.Context, id uuid.UUID) context.Context {
	if id == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, soulIDCtxKey{}, id)
}

// SoulIDFromContext returns the soul id stashed via WithSoulID, or
// uuid.Nil when the context was never tagged. A Nil result on a write
// path is a routing bug — but it is surfaced by the NOT NULL / FK
// constraint on the tenant table (a loud, logged error) rather than a
// panic, so a single unwired path can't take the whole daemon down
// from inside a background goroutine. Use SoulIDFromContextOK when the
// caller needs to branch on presence without relying on the constraint.
func SoulIDFromContext(ctx context.Context) uuid.UUID {
	v, _ := ctx.Value(soulIDCtxKey{}).(uuid.UUID)
	return v
}

// SoulIDFromContextOK returns the soul id and a found flag.
func SoulIDFromContextOK(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(soulIDCtxKey{}).(uuid.UUID)
	if !ok || v == uuid.Nil {
		return uuid.Nil, false
	}
	return v, true
}
