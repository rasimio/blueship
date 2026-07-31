package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	bs "github.com/rasimio/blueship/internal/core"
)

func (s *Store) Append(ctx context.Context, sessionID string, msg bs.Message) error {
	_, err := s.AppendPersisted(ctx, sessionID, msg)
	return err
}

// AppendPersisted adds a message and returns its exact durable receipt.
func (s *Store) AppendPersisted(ctx context.Context, sessionID string, msg bs.Message) (bs.PersistedMessage, error) {
	blocks := bs.NormalizeContent(msg.Content)
	tokens := bs.EstimateTokens(blocks)
	stored, err := s.appendInternal(ctx, sessionID, msg, blocks, nil, tokens)
	if err != nil {
		return bs.PersistedMessage{}, err
	}
	return bs.PersistedMessage{ID: stored.ID, Role: stored.Role, CreatedAt: stored.CreatedAt}, nil
}

// AppendWithTokens adds a message with a known token count (e.g., from API usage).
// Satisfies core.MessageStore.
func (s *Store) AppendWithTokens(ctx context.Context, sessionID string, msg bs.Message, tokens int) error {
	_, err := s.AppendWithTokensPersisted(ctx, sessionID, msg, tokens)
	return err
}

// AppendWithTokensPersisted adds a token-counted message and returns its exact
// durable receipt.
func (s *Store) AppendWithTokensPersisted(ctx context.Context, sessionID string, msg bs.Message, tokens int) (bs.PersistedMessage, error) {
	blocks := bs.NormalizeContent(msg.Content)
	stored, err := s.appendInternal(ctx, sessionID, msg, blocks, nil, tokens)
	if err != nil {
		return bs.PersistedMessage{}, err
	}
	return bs.PersistedMessage{ID: stored.ID, Role: stored.Role, CreatedAt: stored.CreatedAt}, nil
}

// AppendReturning adds a message and returns the stored Message (for CLI/debug use).
func (s *Store) AppendReturning(ctx context.Context, sessionID string, msg bs.Message) (*Message, error) {
	blocks := bs.NormalizeContent(msg.Content)
	tokens := bs.EstimateTokens(blocks)
	return s.appendInternal(ctx, sessionID, msg, blocks, nil, tokens)
}

// UserMessageAnchor identifies the latest real human message and when it
// became durable. The timestamp lets the gateway distinguish a persisted
// message from newer transport ingress still waiting in a debounce queue.
type UserMessageAnchor struct {
	ID        string
	CreatedAt time.Time
}

// LatestUserMessageAnchor returns the latest real user-message anchor in a
// session. Prompt-only autonomous inputs, tool results, and turn boundaries
// cannot advance it.
func (s *Store) LatestUserMessageAnchor(ctx context.Context, sessionID string) (UserMessageAnchor, error) {
	var anchor UserMessageAnchor
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text, created_at FROM chat_messages
		  WHERE session_id = $1 AND role = 'user' AND tool_use_id IS NULL
		  ORDER BY created_at DESC, id DESC LIMIT 1`,
		sessionID,
	).Scan(&anchor.ID, &anchor.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return UserMessageAnchor{}, nil
		}
		return UserMessageAnchor{}, fmt.Errorf("latest user message anchor: %w", err)
	}
	return anchor, nil
}

// LatestUserMessageID is retained for callers that need only the stable id.
func (s *Store) LatestUserMessageID(ctx context.Context, sessionID string) (string, error) {
	anchor, err := s.LatestUserMessageAnchor(ctx, sessionID)
	return anchor.ID, err
}

// LatestDialogMessageID returns the newest provider-visible dialogue row. It
// is a second optimistic-concurrency token for autonomous turns: an assistant
// notification appended after drafting must invalidate the old draft even
// when the latest human anchor itself did not change.
func (s *Store) LatestDialogMessageID(ctx context.Context, sessionID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text FROM chat_messages
		  WHERE session_id = $1 AND role IN ('user', 'assistant')
		  ORDER BY created_at DESC, id DESC LIMIT 1`,
		sessionID,
	).Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("latest dialog message id: %w", err)
	}
	return id, nil
}

// AppendAutonomousAssistant atomically inserts an invisible turn boundary and
// its assistant message. The boundary prevents write-time consumers from
// treating an assistant-initiated message as the answer to the previous user
// turn, while visible-dialog readers already ignore non-user/assistant roles.
func (s *Store) AppendAutonomousAssistant(
	ctx context.Context,
	attemptID uuid.UUID,
	sessionID, text string,
	tgMessageID int64,
	deliveredAt time.Time,
) error {
	text = strings.TrimSpace(text)
	if attemptID == uuid.Nil || sessionID == "" || text == "" {
		return fmt.Errorf("append autonomous assistant: attempt, session, and text are required")
	}
	if deliveredAt.IsZero() {
		deliveredAt = time.Now()
	}
	boundaryID, assistantID := bs.AutonomousTurnMessageIDs(attemptID)
	boundaryJSON, err := json.Marshal([]bs.ContentBlock{})
	if err != nil {
		return fmt.Errorf("append autonomous assistant: marshal boundary: %w", err)
	}
	assistantBlocks := []bs.ContentBlock{{Type: "text", Text: text}}
	assistantJSON, err := json.Marshal(assistantBlocks)
	if err != nil {
		return fmt.Errorf("append autonomous assistant: marshal message: %w", err)
	}
	boundaryProjection := bs.ProjectMessageForWrite(bs.Message{Role: "turn_boundary"}, nil)
	assistantProjection := bs.ProjectMessageForWrite(bs.Message{
		Role:        "assistant",
		Content:     assistantBlocks,
		VisibleText: &text,
	}, assistantBlocks)
	tokens := bs.EstimateTokens(assistantBlocks)
	var tgMID any
	if tgMessageID != 0 {
		tgMID = tgMessageID
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append autonomous assistant: begin tx: %w", err)
	}
	defer tx.Rollback()

	// transaction_timestamp() is stable inside a PostgreSQL transaction.
	// Offset the invisible boundary explicitly so every created_at-only reader
	// observes boundary before assistant rather than relying on random UUIDs.
	//
	// The cast on $5 is load-bearing. Subtracting an interval from an
	// unannotated parameter makes PostgreSQL infer that parameter as an
	// interval as well, and the statement then fails at plan time against a
	// timestamptz column — for every input, so the whole autonomous append
	// aborted and no boundary row was ever written.
	boundaryResult, err := tx.ExecContext(ctx,
		`INSERT INTO chat_messages
		    (id, soul_id, session_id, role, content, token_estimate, created_at,
		     visible_text, projection_status, projection_reason, projector_version)
		 VALUES ($1, $4::uuid, $2, 'turn_boundary', $3, 0,
		         $5::timestamptz - interval '1 microsecond', $6, $7, $8, $9)
		 ON CONFLICT (id) DO NOTHING`,
		boundaryID, sessionID, boundaryJSON, bs.SoulIDFromContext(ctx), deliveredAt,
		boundaryProjection.VisibleText, boundaryProjection.Status,
		projectionReasonArg(boundaryProjection), boundaryProjection.ProjectorVersion,
	)
	if err != nil {
		return fmt.Errorf("append autonomous assistant: insert boundary: %w", err)
	}
	assistantResult, err := tx.ExecContext(ctx,
		`INSERT INTO chat_messages
		    (id, soul_id, session_id, role, content, token_estimate, tg_message_id, created_at,
		     visible_text, projection_status, projection_reason, projector_version)
		 VALUES ($1, $6::uuid, $2, 'assistant', $3, $4, $5,
		         $7, $8, $9, $10, $11)
		 ON CONFLICT (id) DO NOTHING`,
		assistantID, sessionID, assistantJSON, tokens, tgMID, bs.SoulIDFromContext(ctx), deliveredAt,
		assistantProjection.VisibleText, assistantProjection.Status,
		projectionReasonArg(assistantProjection), assistantProjection.ProjectorVersion,
	)
	if err != nil {
		return fmt.Errorf("append autonomous assistant: insert message: %w", err)
	}
	boundaryInserted, err := boundaryResult.RowsAffected()
	if err != nil {
		return fmt.Errorf("append autonomous assistant: boundary rows affected: %w", err)
	}
	assistantInserted, err := assistantResult.RowsAffected()
	if err != nil {
		return fmt.Errorf("append autonomous assistant: message rows affected: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE chat_sessions
		 SET token_count = token_count + $2,
		     message_count = message_count + $3,
		     updated_at = NOW()
		 WHERE id = $1`,
		sessionID, tokens*int(assistantInserted), boundaryInserted+assistantInserted,
	); err != nil {
		return fmt.Errorf("append autonomous assistant: update session counters: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("append autonomous assistant: commit: %w", err)
	}
	return nil
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

// MessageText returns the text blocks stored for one session-scoped message.
// It is used to recover the processed representation of Telegram attachments
// (for example a video transcript) when a reply target has no raw caption.
func (s *Store) MessageText(ctx context.Context, sessionID, messageID string) (string, error) {
	if sessionID == "" || messageID == "" {
		return "", nil
	}
	var raw json.RawMessage
	err := s.db.QueryRowContext(ctx,
		`SELECT content FROM chat_messages
		  WHERE session_id = $1 AND id = $2
		  LIMIT 1`,
		sessionID, messageID,
	).Scan(&raw)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("get message text: %w", err)
	}

	var blocks []bs.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("decode message text: %w", err)
	}
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n\n"), nil
}

func (s *Store) appendInternal(ctx context.Context, sessionID string, msg bs.Message, blocks []bs.ContentBlock, toolUseID *string, tokens int) (*Message, error) {
	contentJSON, err := json.Marshal(blocks)
	if err != nil {
		return nil, fmt.Errorf("marshal content: %w", err)
	}
	projection := bs.ProjectMessageForWrite(msg, blocks)

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
		     reply_to_message_id, tg_message_id, visible_text, projection_status,
		     projection_reason, projector_version)
		 VALUES ($6::uuid, $1, $2, $3, $4, $5, $7, $8, $9, $10, $11, $12)
		 RETURNING id, session_id, role, content, visible_text, projection_status,
		           projection_reason, projector_version, tool_use_id, token_estimate, created_at`,
		sessionID, msg.Role, contentJSON, toolUseID, tokens, bs.SoulIDFromContext(ctx),
		replyTo, tgMID, projection.VisibleText, projection.Status,
		projectionReasonArg(projection), projection.ProjectorVersion,
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

func projectionReasonArg(projection bs.MessageProjection) any {
	if projection.Reason == "" {
		return nil
	}
	return projection.Reason
}

// Messages returns messages for a session, ordered by creation time (newest last).
func (s *Store) Messages(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 100
	}

	var msgs []Message
	err := s.db.SelectContext(ctx, &msgs,
		`SELECT id, session_id, role, content, visible_text, projection_status,
		        projection_reason, projector_version, tool_use_id, token_estimate, created_at
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
		`SELECT id, session_id, role, content, visible_text, projection_status,
		        projection_reason, projector_version, tool_use_id, token_estimate, created_at
		 FROM chat_messages
		 WHERE session_id = $1
		   AND role IN ('user', 'assistant')
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

	return messagesForAPIFromRows(msgs, maxTokens), nil
}

// DialogMessagesForAPI returns recent visible dialogue for prompt assembly.
// It deliberately strips old tool_use/tool_result transcript blocks so the
// message budget represents user/assistant conversation, not internal scratch.
func (s *Store) DialogMessagesForAPI(ctx context.Context, sessionID string, maxTokens int) ([]bs.Message, error) {
	var msgs []Message
	err := s.db.SelectContext(ctx, &msgs,
		`SELECT id, session_id, role, content, visible_text, projection_status,
		        projection_reason, projector_version, tool_use_id, token_estimate, created_at
		 FROM chat_messages
		 WHERE session_id = $1
		 ORDER BY created_at DESC
		 LIMIT 500`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("get dialog messages for API: %w", err)
	}

	return dialogMessagesForAPIFromRows(msgs, maxTokens), nil
}

// RecentToolObservations returns recent tool results as provenance records for
// prompt context. It reads the persisted transcript but keeps raw tool blocks
// out of visible dialogue history.
func (s *Store) RecentToolObservations(ctx context.Context, sessionID string, since time.Time, limit int) ([]bs.ToolObservation, error) {
	if sessionID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 3
	}
	rowLimit := limit * 12
	if rowLimit < 50 {
		rowLimit = 50
	}
	if rowLimit > 200 {
		rowLimit = 200
	}

	var sinceArg any
	if !since.IsZero() {
		sinceArg = since
	}
	var msgs []Message
	err := s.db.SelectContext(ctx, &msgs,
		`SELECT id, session_id, role, content, visible_text, projection_status,
		        projection_reason, projector_version, tool_use_id, token_estimate, created_at
		 FROM chat_messages
		 WHERE session_id = $1
		   AND ($2::timestamptz IS NULL OR created_at >= $2::timestamptz)
		   AND role IN ('user', 'assistant')
		 ORDER BY created_at DESC
		 LIMIT $3`,
		sessionID, sinceArg, rowLimit,
	)
	if err != nil {
		return nil, fmt.Errorf("recent tool observations: %w", err)
	}

	type partialObservation struct {
		obs       bs.ToolObservation
		toolUseID string
	}
	toolInputs := make(map[string]string)
	toolNames := make(map[string]string)
	partials := make([]partialObservation, 0, limit)

	for _, msg := range msgs {
		blocks := bs.NormalizeContent(msg.ToAPIMessage().Content)
		switch msg.Role {
		case "assistant":
			for _, block := range blocks {
				if block.Type != "tool_use" || block.ID == "" {
					continue
				}
				if block.Name != "" {
					toolNames[block.ID] = block.Name
				}
				if len(block.Input) > 0 {
					toolInputs[block.ID] = strings.TrimSpace(string(block.Input))
				}
			}
		case "user":
			for _, block := range blocks {
				if block.Type != "tool_result" || block.ToolUseID == "" {
					continue
				}
				output := toolContentString(block.Content)
				if output == "" && !block.IsError {
					continue
				}
				partials = append(partials, partialObservation{
					toolUseID: block.ToolUseID,
					obs: bs.ToolObservation{
						Name:      block.Name,
						Output:    output,
						IsError:   block.IsError,
						CreatedAt: msg.CreatedAt,
					},
				})
			}
		}
	}

	out := make([]bs.ToolObservation, 0, limit)
	for _, partial := range partials {
		if partial.obs.Name == "" {
			partial.obs.Name = toolNames[partial.toolUseID]
		}
		partial.obs.Input = toolInputs[partial.toolUseID]
		if partial.obs.Name == "" {
			partial.obs.Name = "unknown_tool"
		}
		out = append(out, partial.obs)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func toolContentString(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(data))
	}
}

func messagesForAPIFromRows(msgs []Message, maxTokens int) []bs.Message {
	msgs = providerMessageRows(msgs)
	if len(msgs) == 0 {
		return nil
	}

	selected := selectMessagesForAPI(msgs, maxTokens)
	if len(selected) == 0 {
		return nil
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

	return result
}

func providerMessageRows(msgs []Message) []Message {
	result := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Role == "user" || msg.Role == "assistant" {
			result = append(result, msg)
		}
	}
	return result
}

func dialogMessagesForAPIFromRows(msgs []Message, maxTokens int) []bs.Message {
	if len(msgs) == 0 {
		return nil
	}

	selected := selectDialogMessagesWithinBudget(msgs, maxTokens)
	if len(selected) == 0 {
		return nil
	}
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	return trimLeadingToUser(selected)
}

func selectMessagesForAPI(msgs []Message, maxTokens int) []Message {
	var selected []Message
	tokenSum := 0
	for _, m := range msgs {
		if maxTokens > 0 && tokenSum+m.TokenEstimate > maxTokens {
			break
		}
		tokenSum += m.TokenEstimate
		selected = append(selected, m)
	}

	required := latestToolResultTurnSize(msgs)
	if required > 0 && len(selected) < required {
		selected = append([]Message(nil), msgs[:required]...)
	}

	if len(selected) == 0 {
		selected = append(selected, msgs[0])
	}

	return selected
}

type dialogMessageCandidate struct {
	msg    bs.Message
	tokens int
}

func selectDialogMessagesWithinBudget(msgs []Message, maxTokens int) []bs.Message {
	var candidates []dialogMessageCandidate
	for _, stored := range msgs {
		candidate, ok := visibleDialogCandidate(stored)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if isTranslationWorkflow(candidates) {
		candidates = applyTranslationHistoryLayout(candidates)
	}
	return selectDialogCandidatesWithinBudget(candidates, maxTokens)
}

func selectDialogCandidatesWithinBudget(candidates []dialogMessageCandidate, maxTokens int) []bs.Message {
	var selected []bs.Message
	tokenSum := 0
	for _, candidate := range candidates {
		if maxTokens > 0 && tokenSum+candidate.tokens > maxTokens {
			if len(selected) == 0 {
				selected = append(selected, candidate.msg)
			}
			break
		}
		tokenSum += candidate.tokens
		selected = append(selected, candidate.msg)
	}
	return selected
}

const translationFullRecentMessages = 8

func applyTranslationHistoryLayout(candidates []dialogMessageCandidate) []dialogMessageCandidate {
	if len(candidates) <= translationFullRecentMessages {
		return candidates
	}
	shaped := make([]dialogMessageCandidate, len(candidates))
	copy(shaped, candidates)
	for i := translationFullRecentMessages; i < len(shaped); i++ {
		shaped[i] = compactOlderTranslationCandidate(shaped[i])
	}
	return shaped
}

func compactOlderTranslationCandidate(candidate dialogMessageCandidate) dialogMessageCandidate {
	text := strings.TrimSpace(dialogMessageText(candidate.msg))
	if text == "" {
		return candidate
	}
	originalRunes := len([]rune(text))
	stub := fmt.Sprintf(
		"[history compacted: older %s turn in translation workflow; original_runes=%d; original_tokens_est=%d; contains_hebrew=%t; preview=%q]",
		candidate.msg.Role,
		originalRunes,
		candidate.tokens,
		containsHebrew(text),
		compactPreview(text, 180),
	)
	blocks := []bs.ContentBlock{{Type: "text", Text: stub}}
	return dialogMessageCandidate{
		msg: bs.Message{
			Role:    candidate.msg.Role,
			Content: stub,
		},
		tokens: bs.EstimateTokens(blocks),
	}
}

func isTranslationWorkflow(candidates []dialogMessageCandidate) bool {
	limit := len(candidates)
	if limit > 6 {
		limit = 6
	}
	for i := 0; i < limit; i++ {
		text := dialogMessageText(candidates[i].msg)
		if text == "" {
			continue
		}
		if containsTranslationCue(text) && (containsHebrew(text) || i == 0) {
			return true
		}
		if containsHebrew(text) && (len([]rune(text)) > 400 || strings.Contains(strings.ToLower(text), "[reply to:")) {
			return true
		}
	}
	return false
}

func containsTranslationCue(text string) bool {
	lower := strings.ToLower(text)
	cues := []string{
		"переведи", "перевод", "перевести", "трансл", "иврит", "hebrew", "translate", "translation", "עברית",
	}
	for _, cue := range cues {
		if strings.Contains(lower, cue) {
			return true
		}
	}
	return false
}

func containsHebrew(text string) bool {
	for _, r := range text {
		if r >= 0x0590 && r <= 0x05FF {
			return true
		}
	}
	return false
}

func dialogMessageText(msg bs.Message) string {
	var parts []string
	for _, block := range bs.NormalizeContent(msg.Content) {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func compactPreview(text string, maxRunes int) string {
	oneLine := strings.Join(strings.Fields(text), " ")
	runes := []rune(oneLine)
	if len(runes) <= maxRunes {
		return oneLine
	}
	if maxRunes <= 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

func visibleDialogCandidate(stored Message) (dialogMessageCandidate, bool) {
	if stored.Role != "user" && stored.Role != "assistant" {
		return dialogMessageCandidate{}, false
	}
	blocks := bs.NormalizeContent(stored.ToAPIMessage().Content)
	cleaned := make([]bs.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "tool_use" || b.Type == "tool_result" {
			continue
		}
		cleaned = append(cleaned, b)
	}
	if len(cleaned) == 0 {
		return dialogMessageCandidate{}, false
	}
	return dialogMessageCandidate{
		msg: bs.Message{
			Role:      stored.Role,
			Content:   contentFromBlocks(cleaned),
			CreatedAt: stored.CreatedAt,
		},
		tokens: bs.EstimateTokens(cleaned),
	}, true
}

func contentFromBlocks(blocks []bs.ContentBlock) any {
	if len(blocks) == 1 && blocks[0].Type == "text" {
		return blocks[0].Text
	}
	return blocks
}

func trimLeadingToUser(msgs []bs.Message) []bs.Message {
	start := 0
	for start < len(msgs) && msgs[start].Role != "user" {
		start++
	}
	if start >= len(msgs) {
		return nil
	}
	return msgs[start:]
}

func latestToolResultTurnSize(msgs []Message) int {
	if len(msgs) == 0 || !messageHasToolResult(msgs[0]) {
		return 0
	}

	resultIDs := messageToolResultIDs(msgs[0])
	seenToolUse := false
	for i := 1; i < len(msgs); i++ {
		if messageHasMatchingToolUse(msgs[i], resultIDs) {
			seenToolUse = true
			continue
		}
		if seenToolUse && msgs[i].Role == "user" && !messageHasToolResult(msgs[i]) {
			return i + 1
		}
	}

	return 0
}

func messageHasToolResult(msg Message) bool {
	for _, block := range messageBlocks(msg) {
		if block.Type == "tool_result" {
			return true
		}
	}
	return false
}

func messageToolResultIDs(msg Message) map[string]bool {
	ids := make(map[string]bool)
	for _, block := range messageBlocks(msg) {
		if block.Type == "tool_result" && block.ToolUseID != "" {
			ids[block.ToolUseID] = true
		}
	}
	return ids
}

func messageHasMatchingToolUse(msg Message, resultIDs map[string]bool) bool {
	for _, block := range messageBlocks(msg) {
		if block.Type != "tool_use" {
			continue
		}
		if len(resultIDs) == 0 || resultIDs[block.ID] {
			return true
		}
	}
	return false
}

func messageBlocks(msg Message) []bs.ContentBlock {
	var blocks []bs.ContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err == nil {
		return blocks
	}
	return nil
}

// AllMessagesForAPI returns ALL messages formatted for Claude API (no LIMIT).
// Used for compaction where we need the full conversation to calculate token totals.
func (s *Store) AllMessagesForAPI(ctx context.Context, sessionID string) ([]bs.Message, error) {
	var msgs []Message
	err := s.db.SelectContext(ctx, &msgs,
		`SELECT id, session_id, role, content, visible_text, projection_status,
		        projection_reason, projector_version, tool_use_id, token_estimate, created_at
		 FROM chat_messages
		 WHERE session_id = $1
		   AND role IN ('user', 'assistant')
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
		`SELECT m.id, m.session_id, m.role, m.content, m.visible_text, m.projection_status,
		        m.projection_reason, m.projector_version, m.tool_use_id, m.token_estimate, m.created_at
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
		SELECT m.id, m.session_id, m.role, m.content, m.visible_text, m.projection_status,
		       m.projection_reason, m.projector_version, m.tool_use_id, m.token_estimate, m.created_at
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
		SELECT DISTINCT ON (session_id) id, session_id, role, content, visible_text,
		       projection_status, projection_reason, projector_version,
		       tool_use_id, token_estimate, created_at
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
