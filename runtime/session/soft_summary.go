package session

import (
	"context"
	"database/sql"
	"fmt"
)

type SoftSummaryStatus struct {
	Should      bool
	TokenCount  int
	NewTokens   int
	LatestMsgID string
}

func (s *Store) SoftSummaryStatus(ctx context.Context, sessionID string, threshold, minNewTokens int) (*SoftSummaryStatus, error) {
	if threshold <= 0 {
		return &SoftSummaryStatus{}, nil
	}
	if minNewTokens <= 0 {
		minNewTokens = threshold
	}

	var tokenCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT token_count FROM chat_sessions WHERE id = $1`,
		sessionID,
	).Scan(&tokenCount); err != nil {
		return nil, fmt.Errorf("load session token_count: %w", err)
	}
	if tokenCount < threshold {
		return &SoftSummaryStatus{TokenCount: tokenCount}, nil
	}

	var latestMsgID string
	if err := s.db.QueryRowContext(ctx,
		`SELECT id::text FROM chat_messages WHERE session_id = $1 ORDER BY created_at DESC LIMIT 1`,
		sessionID,
	).Scan(&latestMsgID); err != nil {
		if err == sql.ErrNoRows {
			return &SoftSummaryStatus{TokenCount: tokenCount}, nil
		}
		return nil, fmt.Errorf("load latest message: %w", err)
	}

	var newTokens int
	if err := s.db.QueryRowContext(ctx, `
		WITH last_summary AS (
			SELECT cm.created_at
			FROM chat_session_summaries css
			JOIN chat_messages cm ON cm.id = css.to_message_id
			WHERE css.session_id = $1
			ORDER BY css.created_at DESC
			LIMIT 1
		)
		SELECT COALESCE(SUM(m.token_estimate), 0)
		FROM chat_messages m
		WHERE m.session_id = $1
		  AND m.created_at > COALESCE((SELECT created_at FROM last_summary), '-infinity'::timestamptz)`,
		sessionID,
	).Scan(&newTokens); err != nil {
		return nil, fmt.Errorf("load new tokens since summary: %w", err)
	}

	return &SoftSummaryStatus{
		Should:      newTokens >= minNewTokens,
		TokenCount:  tokenCount,
		NewTokens:   newTokens,
		LatestMsgID: latestMsgID,
	}, nil
}

// LatestSoftSummaryText returns the newest soft summary for a session, or "".
//
// Until this existed, nothing read the summary column. Every threshold crossing
// spent a summarisation call, stored the result and showed it to no one — the
// only observable effect was that SoftSummaryStatus reset its "new tokens since"
// counter. One production session had 1205 messages and 1.13M tokens summarised
// that way, none of it ever reaching a prompt.
//
// It matters more now than it did: the dialogue window starts at the summary
// boundary, so this text is the only thing carrying what came before it.
func (s *Store) LatestSoftSummaryText(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return "", nil
	}
	var summary string
	err := s.db.QueryRowContext(ctx, `
		SELECT summary
		FROM chat_session_summaries
		WHERE session_id = $1
		ORDER BY created_at DESC
		LIMIT 1`, sessionID).Scan(&summary)
	switch {
	case err == sql.ErrNoRows:
		return "", nil
	case err != nil:
		return "", fmt.Errorf("load latest soft summary: %w", err)
	}
	return summary, nil
}

func (s *Store) AppendSoftSummary(ctx context.Context, sessionID, toMessageID string, messageCount, tokenCount int, summary string) error {
	if summary == "" || sessionID == "" || toMessageID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_session_summaries (
			session_id, user_id, soul_id, source, to_message_id,
			message_count, token_count, summary
		)
		SELECT id, user_id, soul_id, source, $2::uuid, $3, $4, $5
		FROM chat_sessions
		WHERE id = $1`,
		sessionID, toMessageID, messageCount, tokenCount, summary,
	)
	if err != nil {
		return fmt.Errorf("append soft summary: %w", err)
	}
	return nil
}
