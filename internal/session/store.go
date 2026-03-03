package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/core"
	"github.com/jmoiron/sqlx"
)

// Store provides CRUD operations for chat sessions and messages.
type Store struct {
	db *sqlx.DB
}

// NewStore creates a new session Store.
func NewStore(db *sqlx.DB) *Store {
	return &Store{db: db}
}

// Create creates a new chat session.
func (s *Store) Create(ctx context.Context, userID, model string) (*Session, error) {
	var sess Session
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO chat_sessions (user_id, model)
		 VALUES ($1, $2)
		 RETURNING id, user_id, title, model, system_prompt_hash,
		           token_count, message_count, active, created_at, updated_at`,
		userID, model,
	).StructScan(&sess)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &sess, nil
}

// GetOrCreate returns the latest active session for a user, or creates a new one.
func (s *Store) GetOrCreate(ctx context.Context, userID, model string) (*Session, error) {
	var sess Session
	err := s.db.GetContext(ctx, &sess,
		`SELECT id, user_id, title, model, system_prompt_hash,
		        token_count, message_count, active, created_at, updated_at
		 FROM chat_sessions
		 WHERE user_id = $1 AND active = true
		 ORDER BY updated_at DESC
		 LIMIT 1`,
		userID,
	)
	if err == nil {
		return &sess, nil
	}
	return s.Create(ctx, userID, model)
}

// Archive marks a session as inactive.
func (s *Store) Archive(ctx context.Context, sessionID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_sessions SET active = false, updated_at = NOW() WHERE id = $1`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	return nil
}

// Append adds a message to a session.
func (s *Store) Append(ctx context.Context, sessionID string, msg bs.Message) (*Message, error) {
	blocks := bs.NormalizeContent(msg.Content)
	tokens := bs.EstimateTokens(blocks)
	return s.appendInternal(ctx, sessionID, msg.Role, blocks, nil, tokens)
}

// AppendWithTokens adds a message with a known token count (e.g., from API usage).
func (s *Store) AppendWithTokens(ctx context.Context, sessionID string, msg bs.Message, tokens int) (*Message, error) {
	blocks := bs.NormalizeContent(msg.Content)
	return s.appendInternal(ctx, sessionID, msg.Role, blocks, nil, tokens)
}

func (s *Store) appendInternal(ctx context.Context, sessionID, role string, blocks []bs.ContentBlock, toolUseID *string, tokens int) (*Message, error) {
	contentJSON, err := json.Marshal(blocks)
	if err != nil {
		return nil, fmt.Errorf("marshal content: %w", err)
	}

	if toolUseID == nil && role == "user" {
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				id := b.ToolUseID
				toolUseID = &id
				break
			}
		}
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var m Message
	err = tx.QueryRowxContext(ctx,
		`INSERT INTO chat_messages (session_id, role, content, tool_use_id, token_estimate)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, session_id, role, content, tool_use_id, token_estimate, created_at`,
		sessionID, role, contentJSON, toolUseID, tokens,
	).StructScan(&m)
	if err != nil {
		return nil, fmt.Errorf("insert message: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE chat_sessions
		 SET token_count = token_count + $2,
		     message_count = message_count + 1,
		     updated_at = NOW()
		 WHERE id = $1`,
		sessionID, tokens,
	)
	if err != nil {
		return nil, fmt.Errorf("update session counters: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &m, nil
}

// Messages returns messages for a session, ordered by creation time (newest last).
func (s *Store) Messages(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 100
	}

	var msgs []Message
	err := s.db.SelectContext(ctx, &msgs,
		`SELECT id, session_id, role, content, tool_use_id, token_estimate, created_at
		 FROM chat_messages
		 WHERE session_id = $1
		 ORDER BY created_at ASC
		 LIMIT $2`,
		sessionID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get messages: %w", err)
	}
	return msgs, nil
}

// MessagesForAPI returns messages formatted for Claude API within a token budget.
func (s *Store) MessagesForAPI(ctx context.Context, sessionID string, maxTokens int) ([]bs.Message, error) {
	var msgs []Message
	err := s.db.SelectContext(ctx, &msgs,
		`SELECT id, session_id, role, content, tool_use_id, token_estimate, created_at
		 FROM chat_messages
		 WHERE session_id = $1
		 ORDER BY created_at DESC
		 LIMIT 50`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("get messages for API: %w", err)
	}

	if len(msgs) == 0 {
		return nil, nil
	}

	var selected []Message
	tokenSum := 0
	for _, m := range msgs {
		if maxTokens > 0 && tokenSum+m.TokenEstimate > maxTokens {
			break
		}
		tokenSum += m.TokenEstimate
		selected = append(selected, m)
	}

	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}

	result := make([]bs.Message, len(selected))
	for i, m := range selected {
		result[i] = m.ToAPIMessage()
	}

	truncateOlderToolResults(result, 10, 500)
	result = trimOrphanedLeading(result)

	return result, nil
}

// MessagesSince returns messages for a user since a given time (across all sessions).
func (s *Store) MessagesSince(ctx context.Context, userID string, since time.Time) ([]Message, error) {
	var msgs []Message
	err := s.db.SelectContext(ctx, &msgs,
		`SELECT m.id, m.session_id, m.role, m.content, m.tool_use_id, m.token_estimate, m.created_at
		 FROM chat_messages m
		 JOIN chat_sessions s ON s.id = m.session_id
		 WHERE s.user_id = $1 AND m.created_at >= $2
		 ORDER BY m.created_at ASC`,
		userID, since,
	)
	if err != nil {
		return nil, fmt.Errorf("messages since: %w", err)
	}
	return msgs, nil
}

// ListActive returns all active sessions for a user.
func (s *Store) ListActive(ctx context.Context, userID string) ([]Session, error) {
	var sessions []Session
	err := s.db.SelectContext(ctx, &sessions,
		`SELECT id, user_id, title, model, system_prompt_hash,
		        token_count, message_count, active, created_at, updated_at
		 FROM chat_sessions
		 WHERE user_id = $1 AND active = true
		 ORDER BY updated_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list active sessions: %w", err)
	}
	return sessions, nil
}

// --- helpers ---

func truncateOlderToolResults(msgs []bs.Message, keep, maxChars int) {
	cutoff := len(msgs) - keep
	if cutoff <= 0 {
		return
	}
	for i := 0; i < cutoff; i++ {
		blocks, ok := msgs[i].Content.([]bs.ContentBlock)
		if !ok {
			continue
		}
		changed := false
		for j := range blocks {
			if blocks[j].Type != "tool_result" {
				continue
			}
			s, ok := blocks[j].Content.(string)
			if !ok || len(s) <= maxChars {
				continue
			}
			blocks[j].Content = fmt.Sprintf("[truncated, %d chars]", len(s))
			changed = true
		}
		if changed {
			msgs[i].Content = blocks
		}
	}
}

func trimOrphanedLeading(msgs []bs.Message) []bs.Message {
	start := 0
	for start < len(msgs) {
		msg := msgs[start]
		if msg.Role != "user" {
			start++
			continue
		}
		if hasToolResult(msg) {
			start++
			continue
		}
		break
	}
	if start >= len(msgs) {
		return nil
	}
	return msgs[start:]
}

func hasToolResult(msg bs.Message) bool {
	blocks := bs.NormalizeContent(msg.Content)
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

func hasToolUse(msg bs.Message) bool {
	blocks := bs.NormalizeContent(msg.Content)
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}

// extractText returns concatenated text from response content blocks.
func ExtractText(content []bs.ContentBlock) string {
	var parts []string
	for _, block := range content {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}
