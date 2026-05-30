package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	bs "github.com/rasimio/blueship/internal/core"
)

func (s *Store) Append(ctx context.Context, sessionID string, msg bs.Message) error {
	blocks := bs.NormalizeContent(msg.Content)
	tokens := bs.EstimateTokens(blocks)
	_, err := s.appendInternal(ctx, sessionID, msg, blocks, nil, tokens)
	return err
}

// AppendWithTokens adds a message with a known token count (e.g., from API usage).
// Satisfies core.MessageStore.
func (s *Store) AppendWithTokens(ctx context.Context, sessionID string, msg bs.Message, tokens int) error {
	blocks := bs.NormalizeContent(msg.Content)
	_, err := s.appendInternal(ctx, sessionID, msg, blocks, nil, tokens)
	return err
}

// AppendReturning adds a message and returns the stored Message (for CLI/debug use).
func (s *Store) AppendReturning(ctx context.Context, sessionID string, msg bs.Message) (*Message, error) {
	blocks := bs.NormalizeContent(msg.Content)
	tokens := bs.EstimateTokens(blocks)
	return s.appendInternal(ctx, sessionID, msg, blocks, nil, tokens)
}

// LookupByTGMessageID finds the chat_messages row id for a Telegram
// reply target. session-scoped so a tg_message_id from another chat
// (same numeric value, different conversation) doesn't false-match.
// Returns ("", nil) when not found — caller treats that as "parent
// pre-dates the relational reply path" and falls back to the inline
// text quote.
func (s *Store) LookupByTGMessageID(ctx context.Context, sessionID string, tgMessageID int64) (string, error) {
	if tgMessageID == 0 {
		return "", nil
	}
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text FROM chat_messages
		  WHERE session_id = $1 AND tg_message_id = $2
		  ORDER BY created_at DESC LIMIT 1`,
		sessionID, tgMessageID,
	).Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("lookup tg message: %w", err)
	}
	return id, nil
}

func (s *Store) appendInternal(ctx context.Context, sessionID string, msg bs.Message, blocks []bs.ContentBlock, toolUseID *string, tokens int) (*Message, error) {
	contentJSON, err := json.Marshal(blocks)
	if err != nil {
		return nil, fmt.Errorf("marshal content: %w", err)
	}

	if toolUseID == nil && msg.Role == "user" {
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				id := b.ToolUseID
				toolUseID = &id
				break
			}
		}
	}

	// Relational reply metadata. Nullable on both ends — empty
	// string and zero TGMessageID land as NULL so non-reply turns
	// don't pollute the column.
	var replyTo any
	if msg.ReplyToMessageID != "" {
		replyTo = msg.ReplyToMessageID
	}
	var tgMID any
	if msg.TGMessageID != 0 {
		tgMID = msg.TGMessageID
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var m Message
	err = tx.QueryRowxContext(ctx,
		`INSERT INTO chat_messages
		    (soul_id, session_id, role, content, tool_use_id, token_estimate,
		     reply_to_message_id, tg_message_id)
		 VALUES ($6::uuid, $1, $2, $3, $4, $5, $7, $8)
		 RETURNING id, session_id, role, content, tool_use_id, token_estimate, created_at`,
		sessionID, msg.Role, contentJSON, toolUseID, tokens, bs.SoulIDFromContext(ctx),
		replyTo, tgMID,
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
		 LIMIT 200`,
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
	result = sanitizeOrphanedToolUse(result)

	return result, nil
}

// AllMessagesForAPI returns ALL messages formatted for Claude API (no LIMIT).
// Used for compaction where we need the full conversation to calculate token totals.
func (s *Store) AllMessagesForAPI(ctx context.Context, sessionID string) ([]bs.Message, error) {
	var msgs []Message
	err := s.db.SelectContext(ctx, &msgs,
		`SELECT id, session_id, role, content, tool_use_id, token_estimate, created_at
		 FROM chat_messages
		 WHERE session_id = $1
		 ORDER BY created_at ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("get all messages for API: %w", err)
	}

	if len(msgs) == 0 {
		return nil, nil
	}

	result := make([]bs.Message, len(msgs))
	for i, m := range msgs {
		result[i] = m.ToAPIMessage()
	}

	return result, nil
}

// MessagesSince returns messages for a user since a given time (across user chat sessions only).
// Excludes background task sessions (source != 'chat') to avoid polluting summaries.
// Satisfies core.SessionQuerier (returns []bs.SessionMessage).
func (s *Store) MessagesSince(ctx context.Context, userID string, since time.Time) ([]bs.SessionMessage, error) {
	var msgs []bs.SessionMessage
	err := s.db.SelectContext(ctx, &msgs,
		`SELECT m.id, m.session_id, m.role, m.content, m.tool_use_id, m.token_estimate, m.created_at
		 FROM chat_messages m
		 JOIN chat_sessions s ON s.id = m.session_id
		 WHERE s.user_id = $1 AND m.created_at >= $2 AND s.source = 'chat'
		 ORDER BY m.created_at ASC`,
		userID, since,
	)
	if err != nil {
		return nil, fmt.Errorf("messages since: %w", err)
	}
	return msgs, nil
}

// ChainMessages returns messages across a session chain (following previous_id links).
// Messages are returned newest-first for cursor pagination.
// Use `before` (created_at timestamp) as cursor; pass zero time for first page.
func (s *Store) ChainMessages(ctx context.Context, sessionID string, limit int, before time.Time) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}

	query := `
		WITH RECURSIVE chain AS (
			SELECT id FROM chat_sessions WHERE id = $1
			UNION ALL
			SELECT cs.previous_id FROM chat_sessions cs
			JOIN chain c ON c.id = cs.id
			WHERE cs.previous_id IS NOT NULL
		)
		SELECT m.id, m.session_id, m.role, m.content, m.tool_use_id, m.token_estimate, m.created_at
		FROM chat_messages m
		WHERE m.session_id IN (SELECT id FROM chain)`

	args := []any{sessionID}
	if !before.IsZero() {
		query += ` AND m.created_at < $2 ORDER BY m.created_at DESC LIMIT $3`
		args = append(args, before, limit)
	} else {
		query += ` ORDER BY m.created_at DESC LIMIT $2`
		args = append(args, limit)
	}

	var msgs []Message
	if err := s.db.SelectContext(ctx, &msgs, query, args...); err != nil {
		return nil, fmt.Errorf("chain messages: %w", err)
	}

	// Reverse to chronological order
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// LastMessages returns the most recent user/assistant message for each of the given session IDs.
func (s *Store) LastMessages(ctx context.Context, sessionIDs []string) (map[string]Message, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	query, args, err := sqlx.In(`
		SELECT DISTINCT ON (session_id) id, session_id, role, content, tool_use_id, token_estimate, created_at
		FROM chat_messages
		WHERE session_id IN (?) AND role IN ('user', 'assistant')
		ORDER BY session_id, created_at DESC`, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("build IN query: %w", err)
	}
	query = s.db.Rebind(query)

	var msgs []Message
	if err := s.db.SelectContext(ctx, &msgs, query, args...); err != nil {
		return nil, fmt.Errorf("last messages: %w", err)
	}

	result := make(map[string]Message, len(msgs))
	for _, m := range msgs {
		result[m.SessionID] = m
	}
	return result, nil
}

// ListActive returns all active sessions for a (user, soul).
func (s *Store) ListActive(ctx context.Context, userID string) ([]Session, error) {
	var sessions []Session
	err := s.db.SelectContext(ctx, &sessions,
		`SELECT * FROM chat_sessions
		 WHERE user_id = $1 AND soul_id = $2 AND active = true
		 ORDER BY updated_at DESC`,
		userID, bs.SoulIDFromContext(ctx),
	)
	if err != nil {
		return nil, fmt.Errorf("list active sessions: %w", err)
	}
	return sessions, nil
}

// CompactSession persists compaction results: deletes old messages, saves summary, recalculates counters.
