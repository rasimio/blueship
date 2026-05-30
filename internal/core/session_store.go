package core

import (
	"context"
	"encoding/json"
	"time"
)

// SessionMessage represents a message from the chat_messages table.
// Used by SessionQuerier to return messages without importing the session package.
type SessionMessage struct {
	ID            string          `db:"id" json:"id"`
	SessionID     string          `db:"session_id" json:"session_id"`
	Role          string          `db:"role" json:"role"`
	Content       json.RawMessage `db:"content" json:"content"`
	ToolUseID     *string         `db:"tool_use_id" json:"tool_use_id,omitempty"`
	TokenEstimate int             `db:"token_estimate" json:"token_estimate"`
	CreatedAt     time.Time       `db:"created_at" json:"created_at"`
}

// SessionQuerier provides read access to chat sessions and messages.
// Defined in core to avoid an import cycle (session imports core).
// Implementations: session.Store.
type SessionQuerier interface {
	// MessagesSince returns messages for a user since a given time (across all sessions).
	MessagesSince(ctx context.Context, userID string, since time.Time) ([]SessionMessage, error)
}
