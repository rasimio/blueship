package core

import "context"

// MessageStore abstracts message persistence for the agent loop.
// Implementations: session.Store (PostgreSQL/BlueShip), or any custom store.
type MessageStore interface {
	// Append persists a message to the session.
	Append(ctx context.Context, sessionID string, msg Message) error

	// AppendWithTokens persists a message with an explicit token count.
	AppendWithTokens(ctx context.Context, sessionID string, msg Message, tokens int) error

	// MessagesForAPI loads recent messages fitting within maxTokens budget.
	MessagesForAPI(ctx context.Context, sessionID string, maxTokens int) ([]Message, error)

	// AllMessagesForAPI loads all messages for a session (used for compaction).
	AllMessagesForAPI(ctx context.Context, sessionID string) ([]Message, error)

	// CompactSession replaces old messages with a summary, keeping keepCount recent messages.
	CompactSession(ctx context.Context, sessionID string, summary string, keepCount int) error

	// CreateSession creates a new chat session, returning its ID.
	CreateSession(ctx context.Context, userID, model string) (sessionID string, err error)
}
