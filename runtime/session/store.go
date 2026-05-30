package session

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"

	bs "github.com/rasimio/blueship/internal/core"
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

// CreateWithPrevious creates a new chat-source session linked to a previous
// one. The 'chat' source tag is what `GetOrCreate` filters on — without it
// the returned session is invisible to the normal lookup path and the next
// message goes to a different session entirely (`/reset` was silently
// broken for this exact reason).
//
// For non-chat sessions (agent_task background runs, etc.) use
// [CreateSessionWithSource] directly — that path is unchanged.
func (s *Store) CreateWithPrevious(ctx context.Context, userID, model, previousID string) (*Session, error) {
	var sess Session
	var err error
	if previousID != "" {
		// Archive the previous session
		s.db.ExecContext(ctx,
			`UPDATE chat_sessions SET active = false, updated_at = NOW() WHERE id = $1`,
			previousID)
		err = s.db.QueryRowxContext(ctx,
			`INSERT INTO chat_sessions (soul_id, user_id, model, previous_id, source)
			 VALUES ($4::uuid, $1, $2, $3, 'chat')
			 RETURNING *`,
			userID, model, previousID, bs.SoulIDFromContext(ctx),
		).StructScan(&sess)
	} else {
		err = s.db.QueryRowxContext(ctx,
			`INSERT INTO chat_sessions (soul_id, user_id, model, source)
			 VALUES ($3::uuid, $1, $2, 'chat')
			 RETURNING *`,
			userID, model, bs.SoulIDFromContext(ctx),
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
			`INSERT INTO chat_sessions (soul_id, user_id, model, source, source_id)
			 VALUES ($5::uuid, $1, $2, $3, $4)
			 RETURNING *`,
			userID, model, source, sourceID, bs.SoulIDFromContext(ctx),
		).StructScan(&sess)
	} else {
		err = s.db.QueryRowxContext(ctx,
			`INSERT INTO chat_sessions (soul_id, user_id, model, source)
			 VALUES ($4::uuid, $1, $2, $3)
			 RETURNING *`,
			userID, model, source, bs.SoulIDFromContext(ctx),
		).StructScan(&sess)
	}
	if err != nil {
		return "", fmt.Errorf("create session with source: %w", err)
	}
	return sess.ID, nil
}

// GetOrCreate returns the latest active session for a (user, soul), or
// creates a new one. The lookup is scoped to the request's soul — without
// it a user with more than one soul collides their sessions.
func (s *Store) GetOrCreate(ctx context.Context, userID, model string) (*Session, error) {
	var sess Session
	err := s.db.GetContext(ctx, &sess,
		`SELECT * FROM chat_sessions
		 WHERE user_id = $1 AND soul_id = $2 AND active = true AND source = 'chat'
		 ORDER BY updated_at DESC
		 LIMIT 1`,
		userID, bs.SoulIDFromContext(ctx),
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

// RecordLastInputTokens stamps the most recent cortex input_tokens onto
// the session. Surfaced by /api/chat/history so the cabinet's token-
// window chip has a value to show on page load. Satisfies core.MessageStore.
func (s *Store) RecordLastInputTokens(ctx context.Context, sessionID string, inputTokens int) error {
	if sessionID == "" || inputTokens <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_sessions SET last_input_tokens = $1, updated_at = NOW() WHERE id = $2`,
		inputTokens, sessionID,
	)
	if err != nil {
		return fmt.Errorf("record last_input_tokens: %w", err)
	}
	return nil
}

// LatestAssistantMessageID returns the ID of the most recent assistant message
// in the session, or "" if none exists. Satisfies core.MessageStore.
func (s *Store) LatestAssistantMessageID(ctx context.Context, sessionID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text FROM chat_messages
		  WHERE session_id = $1 AND role = 'assistant'
		  ORDER BY created_at DESC LIMIT 1`,
		sessionID,
	).Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("latest assistant message id: %w", err)
	}
	return id, nil
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
