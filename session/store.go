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
	return s.CreateWithPrevious(ctx, userID, model, "")
}

// CreateWithPrevious creates a new session linked to a previous one.
func (s *Store) CreateWithPrevious(ctx context.Context, userID, model, previousID string) (*Session, error) {
	var sess Session
	var err error
	if previousID != "" {
		// Archive the previous session
		s.db.ExecContext(ctx,
			`UPDATE chat_sessions SET active = false, updated_at = NOW() WHERE id = $1`,
			previousID)
		err = s.db.QueryRowxContext(ctx,
			`INSERT INTO chat_sessions (user_id, model, previous_id)
			 VALUES ($1, $2, $3)
			 RETURNING *`,
			userID, model, previousID,
		).StructScan(&sess)
	} else {
		err = s.db.QueryRowxContext(ctx,
			`INSERT INTO chat_sessions (user_id, model)
			 VALUES ($1, $2)
			 RETURNING *`,
			userID, model,
		).StructScan(&sess)
	}
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &sess, nil
}

// CreateSession creates a new chat session, returning its ID. Satisfies core.MessageStore.
func (s *Store) CreateSession(ctx context.Context, userID, model string) (string, error) {
	sess, err := s.Create(ctx, userID, model)
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// CreateSessionWithSource creates a session tagged with source and optional source_id.
func (s *Store) CreateSessionWithSource(ctx context.Context, userID, model, source, sourceID string) (string, error) {
	var sess Session
	var err error
	if sourceID != "" {
		err = s.db.QueryRowxContext(ctx,
			`INSERT INTO chat_sessions (user_id, model, source, source_id)
			 VALUES ($1, $2, $3, $4)
			 RETURNING *`,
			userID, model, source, sourceID,
		).StructScan(&sess)
	} else {
		err = s.db.QueryRowxContext(ctx,
			`INSERT INTO chat_sessions (user_id, model, source)
			 VALUES ($1, $2, $3)
			 RETURNING *`,
			userID, model, source,
		).StructScan(&sess)
	}
	if err != nil {
		return "", fmt.Errorf("create session with source: %w", err)
	}
	return sess.ID, nil
}

// GetOrCreate returns the latest active session for a user, or creates a new one.
func (s *Store) GetOrCreate(ctx context.Context, userID, model string) (*Session, error) {
	var sess Session
	err := s.db.GetContext(ctx, &sess,
		`SELECT * FROM chat_sessions
		 WHERE user_id = $1 AND active = true AND source = 'chat'
		 ORDER BY updated_at DESC
		 LIMIT 1`,
		userID,
	)
	if err == nil {
		return &sess, nil
	}
	return s.Create(ctx, userID, model)
}

// IsActive checks if a session exists and is active.
func (s *Store) IsActive(ctx context.Context, sessionID string) (bool, error) {
	var active bool
	err := s.db.GetContext(ctx, &active,
		`SELECT active FROM chat_sessions WHERE id = $1`, sessionID)
	if err != nil {
		return false, err
	}
	return active, nil
}

// ArchiveSession marks a session as inactive. Satisfies core.MessageStore.
func (s *Store) ArchiveSession(ctx context.Context, sessionID string) error {
	return s.Archive(ctx, sessionID)
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
// Satisfies core.MessageStore.
func (s *Store) Append(ctx context.Context, sessionID string, msg bs.Message) error {
	blocks := bs.NormalizeContent(msg.Content)
	tokens := bs.EstimateTokens(blocks)
	_, err := s.appendInternal(ctx, sessionID, msg.Role, blocks, nil, tokens)
	return err
}

// AppendWithTokens adds a message with a known token count (e.g., from API usage).
// Satisfies core.MessageStore.
func (s *Store) AppendWithTokens(ctx context.Context, sessionID string, msg bs.Message, tokens int) error {
	blocks := bs.NormalizeContent(msg.Content)
	_, err := s.appendInternal(ctx, sessionID, msg.Role, blocks, nil, tokens)
	return err
}

// AppendReturning adds a message and returns the stored Message (for CLI/debug use).
func (s *Store) AppendReturning(ctx context.Context, sessionID string, msg bs.Message) (*Message, error) {
	blocks := bs.NormalizeContent(msg.Content)
	tokens := bs.EstimateTokens(blocks)
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

// ListActive returns all active sessions for a user.
func (s *Store) ListActive(ctx context.Context, userID string) ([]Session, error) {
	var sessions []Session
	err := s.db.SelectContext(ctx, &sessions,
		`SELECT * FROM chat_sessions
		 WHERE user_id = $1 AND active = true
		 ORDER BY updated_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list active sessions: %w", err)
	}
	return sessions, nil
}

// CompactSession persists compaction results: deletes old messages, saves summary, recalculates counters.
func (s *Store) CompactSession(ctx context.Context, sessionID string, summary string, keepCount int) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Delete all messages except the most recent keepCount
	_, err = tx.ExecContext(ctx, `
		DELETE FROM chat_messages
		WHERE session_id = $1
		  AND id NOT IN (
		      SELECT id FROM chat_messages
		      WHERE session_id = $1
		      ORDER BY created_at DESC
		      LIMIT $2
		  )`, sessionID, keepCount)
	if err != nil {
		return fmt.Errorf("delete old messages: %w", err)
	}

	// 2. Append summary + recalculate counters
	_, err = tx.ExecContext(ctx, `
		UPDATE chat_sessions SET
		    compact_summary = CASE
		        WHEN compact_summary IS NULL OR compact_summary = '' THEN $2
		        ELSE compact_summary || E'\n\n---\n\n' || $2
		    END,
		    token_count = (SELECT COALESCE(SUM(token_estimate), 0) FROM chat_messages WHERE session_id = $1),
		    message_count = (SELECT COUNT(*) FROM chat_messages WHERE session_id = $1),
		    updated_at = NOW()
		WHERE id = $1`, sessionID, summary)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	return tx.Commit()
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

// sanitizeOrphanedToolUse removes tool_use blocks from assistant messages
// that have no matching tool_result in the next user message. This happens when
// a thinking/heartbeat job crashes mid-tool-execution. Works anywhere in the
// conversation, not just trailing messages.
func sanitizeOrphanedToolUse(msgs []bs.Message) []bs.Message {
	for i := 0; i < len(msgs); i++ {
		if msgs[i].Role != "assistant" || !hasToolUse(msgs[i]) {
			continue
		}

		// Collect tool_use IDs from this assistant message
		blocks := bs.NormalizeContent(msgs[i].Content)
		toolUseIDs := make(map[string]bool)
		for _, b := range blocks {
			if b.Type == "tool_use" && b.ID != "" {
				toolUseIDs[b.ID] = true
			}
		}

		// Check if next message has matching tool_results
		if i+1 < len(msgs) {
			nextBlocks := bs.NormalizeContent(msgs[i+1].Content)
			for _, b := range nextBlocks {
				if b.Type == "tool_result" && b.ToolUseID != "" {
					delete(toolUseIDs, b.ToolUseID)
				}
			}
		}

		// If all tool_use have results, this message is fine
		if len(toolUseIDs) == 0 {
			continue
		}

		// Strip orphaned tool_use blocks, keep text blocks
		var cleaned []bs.ContentBlock
		for _, b := range blocks {
			if b.Type == "tool_use" && toolUseIDs[b.ID] {
				continue // drop orphaned tool_use
			}
			cleaned = append(cleaned, b)
		}

		if len(cleaned) == 0 {
			// Entire message was tool_use — remove it
			msgs = append(msgs[:i], msgs[i+1:]...)
			i-- // re-check this index
		} else {
			msgs[i].Content = cleaned
		}
	}
	return msgs
}

// Verify Store satisfies core.SessionQuerier at compile time.
var _ bs.SessionQuerier = (*Store)(nil)

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
