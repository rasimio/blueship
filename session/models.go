package session

import (
	"encoding/json"
	"time"

	bs "github.com/rasimio/blueship/core"
)

// Session represents a chat session stored in PostgreSQL.
type Session struct {
	ID               string    `db:"id" json:"id"`
	UserID           string    `db:"user_id" json:"user_id"`
	Title            *string   `db:"title" json:"title,omitempty"`
	Model            string    `db:"model" json:"model"`
	SystemPromptHash *string   `db:"system_prompt_hash" json:"-"`
	TokenCount       int       `db:"token_count" json:"token_count"`
	MessageCount     int       `db:"message_count" json:"message_count"`
	CompactSummary   *string   `db:"compact_summary" json:"compact_summary,omitempty"`
	PreviousID       *string   `db:"previous_id" json:"previous_id,omitempty"`
	Active           bool      `db:"active" json:"active"`
	Source           string    `db:"source" json:"source"`
	SourceID         *string   `db:"source_id" json:"source_id,omitempty"`
	CreatedAt        time.Time `db:"created_at" json:"created_at"`
	UpdatedAt        time.Time `db:"updated_at" json:"updated_at"`
}

// Message represents a chat message stored in PostgreSQL.
type Message struct {
	ID            string          `db:"id" json:"id"`
	SessionID     string          `db:"session_id" json:"session_id"`
	Role          string          `db:"role" json:"role"`
	Content       json.RawMessage `db:"content" json:"content"`
	ToolUseID     *string         `db:"tool_use_id" json:"tool_use_id,omitempty"`
	TokenEstimate int             `db:"token_estimate" json:"token_estimate"`
	CreatedAt     time.Time       `db:"created_at" json:"created_at"`
}

// ToAPIMessage converts a stored Message back to a bs.Message.
func (m *Message) ToAPIMessage() bs.Message {
	var content any

	var blocks []bs.ContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		if len(blocks) == 1 && blocks[0].Type == "text" {
			content = blocks[0].Text
		} else {
			content = blocks
		}
	} else {
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			content = s
		} else {
			content = string(m.Content)
		}
	}

	return bs.Message{
		Role:    m.Role,
		Content: content,
	}
}
