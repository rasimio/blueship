package session

import (
	"context"
	"fmt"
	"strings"

	bs "github.com/rasimio/blueship/internal/core"
)

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
