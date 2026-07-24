package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// AgentTaskStore provides CRUD and polling queries for agent_tasks.
type AgentTaskStore struct {
	db *sqlx.DB
}

// NewAgentTaskStore creates a store backed by the agent_tasks table.
func NewAgentTaskStore(db *sqlx.DB) *AgentTaskStore {
	return &AgentTaskStore{db: db}
}

type taskDeliveryRow struct {
	InputID string `db:"input_id"`
	ItemKey string `db:"item_key"`
}

const taskDeliveryInputIDMaxBytes = 64

// LookupDelivered returns refs that are either confirmed delivered or owned by
// a durable notification attempt in any state. Retryable attempts are drained
// from their immutable journal text; task programs must never regenerate them.
func (s *AgentTaskStore) LookupDelivered(ctx context.Context, taskID uuid.UUID, refs []TaskDeliveryRef) (map[TaskDeliveryRef]bool, error) {
	refs, err := normalizeTaskDeliveryRefs(refs)
	if err != nil {
		return nil, err
	}
	delivered := make(map[TaskDeliveryRef]bool, len(refs))
	if len(refs) == 0 {
		return delivered, nil
	}
	inputIDs := make([]string, 0, len(refs))
	itemKeys := make([]string, 0, len(refs))
	for _, ref := range refs {
		inputIDs = append(inputIDs, ref.InputID)
		itemKeys = append(itemKeys, ref.ItemKey)
	}
	var rows []taskDeliveryRow
	if err := s.db.SelectContext(ctx, &rows, `
		SELECT d.input_id, d.item_key
		FROM agent_task_deliveries AS d
		JOIN unnest($2::text[], $3::text[]) AS wanted(input_id, item_key)
		  ON wanted.input_id = d.input_id AND wanted.item_key = d.item_key
		WHERE d.task_id = $1
		UNION
		SELECT i.input_id, i.item_key
		FROM agent_task_notification_attempt_items AS i
		JOIN unnest($2::text[], $3::text[]) AS wanted(input_id, item_key)
		  ON wanted.input_id = i.input_id AND wanted.item_key = i.item_key
		WHERE i.task_id = $1`, taskID, pq.Array(inputIDs), pq.Array(itemKeys)); err != nil {
		return nil, err
	}
	for _, row := range rows {
		delivered[TaskDeliveryRef{InputID: row.InputID, ItemKey: row.ItemKey}] = true
	}
	return delivered, nil
}

// MarkDelivered records refs idempotently after their notification was sent.
func (s *AgentTaskStore) MarkDelivered(ctx context.Context, taskID uuid.UUID, refs []TaskDeliveryRef) error {
	refs, err := normalizeTaskDeliveryRefs(refs)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	inputIDs := make([]string, 0, len(refs))
	itemKeys := make([]string, 0, len(refs))
	for _, ref := range refs {
		inputIDs = append(inputIDs, ref.InputID)
		itemKeys = append(itemKeys, ref.ItemKey)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_task_deliveries (task_id, input_id, item_key)
		SELECT $1, wanted.input_id, wanted.item_key
		FROM unnest($2::text[], $3::text[]) AS wanted(input_id, item_key)
		ON CONFLICT (task_id, input_id, item_key) DO NOTHING`,
		taskID, pq.Array(inputIDs), pq.Array(itemKeys))
	return err
}

func normalizeTaskDeliveryRefs(refs []TaskDeliveryRef) ([]TaskDeliveryRef, error) {
	seen := make(map[TaskDeliveryRef]struct{}, len(refs))
	out := make([]TaskDeliveryRef, 0, len(refs))
	for _, ref := range refs {
		ref.InputID = strings.TrimSpace(ref.InputID)
		ref.ItemKey = strings.TrimSpace(ref.ItemKey)
		if ref.InputID == "" || ref.ItemKey == "" {
			continue
		}
		if len(ref.InputID) > taskDeliveryInputIDMaxBytes {
			return nil, fmt.Errorf("task delivery input_id exceeds %d bytes", taskDeliveryInputIDMaxBytes)
		}
		if len(ref.ItemKey) > TaskDeliveryItemKeyMaxBytes {
			return nil, fmt.Errorf("task delivery item_key exceeds %d bytes", TaskDeliveryItemKeyMaxBytes)
		}
		if _, exists := seen[ref]; exists {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	return out, nil
}

// BeginNotificationAttempt atomically persists an immutable notification
// intent and reserves every keyed occurrence before any external transport is
// called. created=false means this exact occurrence set already exists. A
// partial overlap is returned as an error after rolling back the whole new
// attempt, allowing a later scheduled execution to rebuild from unreserved refs.
func (s *AgentTaskStore) BeginNotificationAttempt(
	ctx context.Context,
	taskID, userID uuid.UUID,
	text string,
	refs []TaskDeliveryRef,
) (uuid.UUID, bool, error) {
	if taskID == uuid.Nil || userID == uuid.Nil {
		return uuid.Nil, false, fmt.Errorf("begin notification attempt: task_id and user_id are required")
	}
	if strings.TrimSpace(text) == "" {
		return uuid.Nil, false, fmt.Errorf("begin notification attempt: text is required")
	}
	refs, err := normalizeTaskDeliveryRefs(refs)
	if err != nil {
		return uuid.Nil, false, err
	}
	if len(refs) == 0 {
		return uuid.Nil, false, fmt.Errorf("begin notification attempt: at least one delivery ref is required")
	}
	sortTaskDeliveryRefs(refs)
	occurrenceKey := taskNotificationOccurrenceKey(refs)

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("begin notification attempt transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	attemptID := uuid.New()
	var insertedID uuid.UUID
	err = tx.GetContext(ctx, &insertedID, `
		INSERT INTO agent_task_notification_attempts (
			id, task_id, user_id, occurrence_key, message_text
		)
		SELECT $1, t.id, t.user_id, $4, $5
		FROM agent_tasks AS t
		WHERE t.id = $2 AND t.user_id = $3
		ON CONFLICT (task_id, occurrence_key) DO NOTHING
		RETURNING id`, attemptID, taskID, userID, occurrenceKey, text)
	if err == sql.ErrNoRows {
		// Either this exact occurrence was already journaled or the supplied
		// task/user pair does not exist. Only the former is an idempotent hit.
		if getErr := tx.GetContext(ctx, &attemptID, `
			SELECT id
			FROM agent_task_notification_attempts
			WHERE task_id = $1 AND user_id = $2 AND occurrence_key = $3`,
			taskID, userID, occurrenceKey); getErr != nil {
			if getErr == sql.ErrNoRows {
				return uuid.Nil, false, fmt.Errorf("begin notification attempt: task/user pair not found")
			}
			return uuid.Nil, false, fmt.Errorf("lookup existing notification attempt: %w", getErr)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return uuid.Nil, false, fmt.Errorf("commit existing notification attempt lookup: %w", commitErr)
		}
		return attemptID, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("insert notification attempt: %w", err)
	}
	attemptID = insertedID

	inputIDs := make([]string, 0, len(refs))
	itemKeys := make([]string, 0, len(refs))
	for _, ref := range refs {
		inputIDs = append(inputIDs, ref.InputID)
		itemKeys = append(itemKeys, ref.ItemKey)
	}
	var reserved []taskDeliveryRow
	if err := tx.SelectContext(ctx, &reserved, `
		INSERT INTO agent_task_notification_attempt_items (
			attempt_id, task_id, input_id, item_key
		)
		SELECT $1, $2, wanted.input_id, wanted.item_key
		FROM unnest($3::text[], $4::text[]) AS wanted(input_id, item_key)
		ON CONFLICT (task_id, input_id, item_key) DO NOTHING
		RETURNING input_id, item_key`,
		attemptID, taskID, pq.Array(inputIDs), pq.Array(itemKeys)); err != nil {
		return uuid.Nil, false, fmt.Errorf("reserve notification items: %w", err)
	}
	if len(reserved) != len(refs) {
		// A differently-grouped concurrent attempt already owns at least one
		// ref. Roll back this whole attempt so no partial reservation survives.
		// This is deliberately not an idempotent hit: a later scheduled execution
		// can dual-read the winning reservations and build from the refs left free.
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return uuid.Nil, false, fmt.Errorf("rollback overlapping notification attempt: %w", rollbackErr)
		}
		return uuid.Nil, false, fmt.Errorf(
			"reserve notification items: partial overlap (%d of %d refs were free)",
			len(reserved), len(refs),
		)
	}

	if err := tx.Commit(); err != nil {
		return uuid.Nil, false, fmt.Errorf("commit notification attempt: %w", err)
	}
	return attemptID, true, nil
}

// ConfirmNotificationAttempt records a provider receipt and the final delivery
// ledger rows in one transaction. A repeated confirm after a lost DB response
// is idempotent; an uncertain attempt can never be promoted to sent.
func (s *AgentTaskStore) ConfirmNotificationAttempt(ctx context.Context, id uuid.UUID, receipt TaskNotificationReceipt) error {
	if id == uuid.Nil {
		return fmt.Errorf("confirm notification attempt: id is required")
	}
	if strings.TrimSpace(receipt.Transport) == "" {
		return fmt.Errorf("confirm notification attempt: receipt transport is required")
	}
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("confirm notification attempt: marshal receipt: %w", err)
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("confirm notification attempt transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	state, err := notificationAttemptStateForUpdate(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("confirm notification attempt: %w", err)
	}
	switch state {
	case "sent":
		return tx.Commit()
	case "uncertain":
		return fmt.Errorf("confirm notification attempt: attempt %s is uncertain", id)
	case "dispatching":
	default:
		return fmt.Errorf("confirm notification attempt: invalid state %q", state)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_task_deliveries (task_id, input_id, item_key)
		SELECT task_id, input_id, item_key
		FROM agent_task_notification_attempt_items
		WHERE attempt_id = $1
		ON CONFLICT (task_id, input_id, item_key) DO NOTHING`, id); err != nil {
		return fmt.Errorf("confirm notification attempt: insert deliveries: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE agent_task_notification_attempts
		SET state = 'sent', receipt = $2::jsonb, error_message = NULL,
		    next_attempt_at = NULL, resolved_at = NOW()
		WHERE id = $1 AND state = 'dispatching'`, id, string(receiptJSON))
	if err != nil {
		return fmt.Errorf("confirm notification attempt: update journal: %w", err)
	}
	if affected, err := res.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return fmt.Errorf("confirm notification attempt: rows affected: %w", err)
		}
		return fmt.Errorf("confirm notification attempt: dispatching state lost")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("confirm notification attempt: commit: %w", err)
	}
	return nil
}

// MarkNotificationUncertain tombstones an ambiguous external outcome. It does
// not release reservation items, so the occurrence remains suppressed forever
// unless an operator explicitly repairs the journal.
func (s *AgentTaskStore) MarkNotificationUncertain(ctx context.Context, id uuid.UUID, reason string) error {
	if id == uuid.Nil {
		return fmt.Errorf("mark notification uncertain: id is required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "notification delivery outcome is uncertain"
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mark notification uncertain transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state, err := notificationAttemptStateForUpdate(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("mark notification uncertain: %w", err)
	}
	switch state {
	case "uncertain":
		return tx.Commit()
	case "sent":
		return fmt.Errorf("mark notification uncertain: attempt %s is already sent", id)
	case "dispatching":
	default:
		return fmt.Errorf("mark notification uncertain: invalid state %q", state)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_task_notification_attempts
		SET state = 'uncertain', error_message = $2,
		    next_attempt_at = NULL, resolved_at = NOW()
		WHERE id = $1 AND state = 'dispatching'`, id, reason); err != nil {
		return fmt.Errorf("mark notification uncertain: update journal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark notification uncertain: commit: %w", err)
	}
	return nil
}

// DeferNotificationAttempt schedules the same immutable message for a durable
// retry after a conclusive, retryable provider rejection. Reservation items
// remain owned by this attempt, so task integrations and the LLM never rerun.
func (s *AgentTaskStore) DeferNotificationAttempt(ctx context.Context, id uuid.UUID, reason string, retryAt time.Time) error {
	if id == uuid.Nil {
		return fmt.Errorf("defer notification attempt: id is required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("defer notification attempt: reason is required")
	}
	if retryAt.IsZero() {
		return fmt.Errorf("defer notification attempt: retry_at is required")
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("defer notification attempt transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state, err := notificationAttemptStateForUpdate(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("defer notification attempt: %w", err)
	}
	switch state {
	case "retryable":
		// Idempotent replay after an ambiguous DB commit response.
		return tx.Commit()
	case "dispatching":
	case "sent", "uncertain", "rejected":
		return fmt.Errorf("defer notification attempt: attempt %s is %s", id, state)
	default:
		return fmt.Errorf("defer notification attempt: invalid state %q", state)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_task_notification_attempts
		SET state = 'retryable', error_message = $2, next_attempt_at = $3,
		    resolved_at = NULL
		WHERE id = $1 AND state = 'dispatching'`, id, reason, retryAt); err != nil {
		return fmt.Errorf("defer notification attempt: update journal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("defer notification attempt: commit: %w", err)
	}
	return nil
}

// RejectNotificationAttempt terminally records a conclusive non-retryable
// provider rejection. The occurrence reservation remains in place forever.
func (s *AgentTaskStore) RejectNotificationAttempt(ctx context.Context, id uuid.UUID, reason string) error {
	if id == uuid.Nil {
		return fmt.Errorf("reject notification attempt: id is required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("reject notification attempt: reason is required")
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reject notification attempt transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state, err := notificationAttemptStateForUpdate(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("reject notification attempt: %w", err)
	}
	switch state {
	case "rejected":
		return tx.Commit()
	case "dispatching":
	case "retryable", "sent", "uncertain":
		return fmt.Errorf("reject notification attempt: attempt %s is %s", id, state)
	default:
		return fmt.Errorf("reject notification attempt: invalid state %q", state)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_task_notification_attempts
		SET state = 'rejected', error_message = $2,
		    next_attempt_at = NULL, resolved_at = NOW()
		WHERE id = $1 AND state = 'dispatching'`, id, reason); err != nil {
		return fmt.Errorf("reject notification attempt: update journal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reject notification attempt: commit: %w", err)
	}
	return nil
}

// ClaimRetryableNotification atomically claims the oldest due retryable intent.
// SKIP LOCKED lets concurrent drainers select disjoint rows without waiting.
// A claimed row returns to dispatching and is never automatically reclaimed if
// the worker crashes, preserving the journal's at-most-once bias.
func (s *AgentTaskStore) ClaimRetryableNotification(ctx context.Context, now time.Time) (*TaskNotificationIntent, error) {
	if now.IsZero() {
		return nil, fmt.Errorf("claim retryable notification: now is required")
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim retryable notification transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var intent TaskNotificationIntent
	err = tx.GetContext(ctx, &intent, `
		WITH candidate AS (
			SELECT id
			FROM agent_task_notification_attempts
			WHERE state = 'retryable' AND next_attempt_at <= $1
			ORDER BY next_attempt_at, created_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE agent_task_notification_attempts AS a
		SET state = 'dispatching', attempt_count = a.attempt_count + 1,
		    last_attempt_at = $1, next_attempt_at = NULL,
		    error_message = NULL, resolved_at = NULL
		FROM candidate
		WHERE a.id = candidate.id
		RETURNING a.id, a.task_id, a.user_id, a.message_text AS text`, now)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim retryable notification: %w", err)
	}
	var rows []taskDeliveryRow
	if err := tx.SelectContext(ctx, &rows, `
		SELECT input_id, item_key
		FROM agent_task_notification_attempt_items
		WHERE attempt_id = $1
		ORDER BY input_id, item_key`, intent.ID); err != nil {
		return nil, fmt.Errorf("claim retryable notification refs: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("claim retryable notification: attempt %s has no reserved refs", intent.ID)
	}
	intent.Refs = make([]TaskDeliveryRef, 0, len(rows))
	for _, row := range rows {
		intent.Refs = append(intent.Refs, TaskDeliveryRef{InputID: row.InputID, ItemKey: row.ItemKey})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claim retryable notification commit: %w", err)
	}
	return &intent, nil
}

func notificationAttemptStateForUpdate(ctx context.Context, tx *sqlx.Tx, id uuid.UUID) (string, error) {
	var state string
	if err := tx.GetContext(ctx, &state, `
		SELECT state
		FROM agent_task_notification_attempts
		WHERE id = $1
		FOR UPDATE`, id); err != nil {
		return "", err
	}
	return state, nil
}

func sortTaskDeliveryRefs(refs []TaskDeliveryRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].InputID != refs[j].InputID {
			return refs[i].InputID < refs[j].InputID
		}
		return refs[i].ItemKey < refs[j].ItemKey
	})
}

func taskNotificationOccurrenceKey(refs []TaskDeliveryRef) string {
	refs = append([]TaskDeliveryRef(nil), refs...)
	sortTaskDeliveryRefs(refs)
	hash := sha256.New()
	_, _ = hash.Write([]byte("task-notification-occurrence/v1\x00"))
	var length [4]byte
	for _, ref := range refs {
		binary.BigEndian.PutUint32(length[:], uint32(len(ref.InputID)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(ref.InputID))
		binary.BigEndian.PutUint32(length[:], uint32(len(ref.ItemKey)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(ref.ItemKey))
	}
	return "refs:v1:" + base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

// PendingTasks returns tasks ready to be picked up by the scheduler.
func (s *AgentTaskStore) PendingTasks(ctx context.Context) ([]AgentTask, error) {
	var tasks []AgentTask
	err := s.db.SelectContext(ctx, &tasks, `
		SELECT * FROM agent_tasks
		WHERE status = 'pending'
		  AND (max_iterations = 0 OR iteration < max_iterations)
		  AND (deadline IS NULL OR deadline > NOW())
		ORDER BY last_run_at NULLS FIRST, created_at`)
	return tasks, err
}

// SetRunning marks a task as running. Does NOT increment iteration —
// iteration is incremented only on successful completion (Complete/UpdateProgress)
// so that crashes don't waste iterations.
func (s *AgentTaskStore) SetRunning(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'running', last_run_at = NOW()
		WHERE id = $1`, id)
	return err
}

// TrySetRunning atomically claims a pending task. The in-process busy map is
// only an optimization; this status predicate is the cross-goroutine/process
// correctness fence that prevents two schedulers from sending the same tick.
func (s *AgentTaskStore) TrySetRunning(ctx context.Context, id uuid.UUID) (bool, error) {
	var claimed uuid.UUID
	err := s.db.GetContext(ctx, &claimed, `
		UPDATE agent_tasks
		SET status = 'running', last_run_at = NOW()
		WHERE id = $1 AND status = 'pending'
		RETURNING id`, id)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, err
}

// UpdateProgress saves intermediate state, increments iteration, and sets
// the task back to pending. Always clears required_recheck_urls so a
// stale recheck list doesn't bleed into the next iteration's Gate B'
// check — the only path that LEAVES recheck URLs set is the explicit
// UpdateProgressWithRecheck branch used by the grounding-failure path.
func (s *AgentTaskStore) UpdateProgress(ctx context.Context, id uuid.UUID, progress json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET progress = $2, status = 'pending', iteration = iteration + 1,
		    required_recheck_urls = '{}'
		WHERE id = $1`, id, progress)
	return err
}

// UpdateProgressWithRecheck is the rejected-by-grounding variant of
// UpdateProgress. Same semantics — progress saved, iteration++, status
// back to pending — but persists a list of URLs the next iteration MUST
// re-fetch before another acceptance attempt is permitted. Gate B' in
// evaluateAcceptance reads this list and hard-fails any submit that
// didn't call browser_fetch on every URL within the same iteration's
// tool_calls trace. Empty recheckURLs collapses to a regular
// UpdateProgress so the existing semantics keep working.
func (s *AgentTaskStore) UpdateProgressWithRecheck(ctx context.Context, id uuid.UUID, progress json.RawMessage, recheckURLs []string) error {
	if len(recheckURLs) == 0 {
		return s.UpdateProgress(ctx, id, progress)
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET progress = $2, status = 'pending', iteration = iteration + 1,
		    required_recheck_urls = $3
		WHERE id = $1`, id, progress, pq.Array(recheckURLs))
	return err
}

// Complete marks a task as done with a final result, increments iteration,
// and clears any pending recheck URLs (a passing submit obviates them).
func (s *AgentTaskStore) Complete(ctx context.Context, id uuid.UUID, result string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'done', result = $2, completed_at = NOW(),
		    iteration = iteration + 1, required_recheck_urls = '{}'
		WHERE id = $1`, id, result)
	return err
}

// CompleteExhausted force-terminates tasks that exhausted max_iterations
// without reaching done on their own. The terminal status depends on the
// task's lifecycle:
//   - recurring (schedule != NULL) — left alone; recurring jobs reset
//     to iteration=0 each cycle via ResetForNextRun and never hit the
//     cap legitimately.
//   - one-shot (schedule == NULL) — marked 'failed' with an explanatory
//     error_message. Auto-completing as 'done' was the old behaviour and
//     was wrong: it claimed success for tasks that, by definition, never
//     met their acceptance criteria.
//
// ExhaustedTask identifies a task CompleteExhausted just force-failed, so the
// scheduler can alert the owner ONCE per truly-dead task (rather than firing an
// alert on every retryable per-iteration failure).
type ExhaustedTask struct {
	ID                 uuid.UUID `db:"id"`
	Title              string    `db:"title"`
	UserID             uuid.UUID `db:"user_id"`
	SoulID             uuid.UUID `db:"soul_id"`
	Iteration          int       `db:"iteration"`
	MaxIterations      int       `db:"max_iterations"`
	AcceptanceFeedback string    `db:"acceptance_feedback"`
	SessionID          string    `db:"session_id"`
	// Output is the best-effort draft salvaged from the last non-empty
	// iteration — delivered to the owner with a caveat instead of nothing.
	Output string `db:"output"`
}

// CompleteExhausted force-fails tasks that hit max_iterations without passing
// the acceptance gate. Rather than discard the work, it SALVAGES the last
// non-empty iteration draft into agent_tasks.result and returns it, so the
// caller can deliver it to the owner with a caveat (the draft already sits in
// agent_task_iterations.output, previously only as forensics).
func (s *AgentTaskStore) CompleteExhausted(ctx context.Context) []ExhaustedTask {
	var failed []ExhaustedTask
	if err := s.db.SelectContext(ctx, &failed, `
		UPDATE agent_tasks t
		SET status = 'failed',
		    completed_at = NOW(),
		    error_message = COALESCE(error_message,
		        'max_iterations reached without satisfying acceptance criteria'),
		    result = COALESCE(
		      CASE
		        WHEN btrim(COALESCE(t.result, '')) <> ''
		         AND t.result <> 'Cancelled by user'
		        THEN t.result
		      END,
		      (
		        SELECT i.output FROM agent_task_iterations i
		         WHERE i.task_id = t.id AND COALESCE(i.output, '') <> ''
		         ORDER BY i.iteration DESC
		         LIMIT 1
		      ),
		      'max_iterations reached without satisfying acceptance criteria'
		    )
		WHERE t.status = 'pending'
		  AND t.schedule IS NULL
		  AND t.max_iterations > 0
		  AND t.iteration >= t.max_iterations
		RETURNING id, title, user_id, soul_id, iteration, max_iterations,
		          COALESCE(progress->>'acceptance_feedback', '') AS acceptance_feedback,
		          COALESCE(progress->>'session_id', '') AS session_id,
		          COALESCE(result, '') AS output`); err != nil {
		return nil
	}
	return failed
}

// UpdateShaping persists the skill router's decision: the stamped config
// (skills + routed marker) and a possibly-tightened iteration cap. Runs
// once per task, before the first iteration.
func (s *AgentTaskStore) UpdateShaping(ctx context.Context, id uuid.UUID, config []byte, maxIterations int, acceptance *string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		   SET config = $2,
		       max_iterations = $3,
		       acceptance_criteria = COALESCE($4, acceptance_criteria)
		 WHERE id = $1`, id, config, maxIterations, acceptance)
	return err
}

// SetPending resets a task back to pending (for retry after transient errors).
func (s *AgentTaskStore) SetPending(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks SET status = 'pending' WHERE id = $1`, id)
	return err
}

// SetPendingForNotificationRetry requeues a recurring task for the very next
// scheduler tick after its user-facing send failed. Clearing last_run_at is
// intentional: waiting the full task cadence can move a time-windowed source
// (for example an upcoming meeting) out of view before the retry.
func (s *AgentTaskStore) SetPendingForNotificationRetry(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'pending', last_run_at = NULL
		WHERE id = $1`, id)
	return err
}

// Fail marks a task as failed with an error message.
func (s *AgentTaskStore) Fail(ctx context.Context, id uuid.UUID, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'failed', error_message = $2
		WHERE id = $1`, id, errMsg)
	return err
}

// Cancel marks a pending or running task as done with cancellation message.
func (s *AgentTaskStore) Cancel(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'done', result = 'Cancelled by user', completed_at = NOW()
		WHERE id = $1 AND status IN ('pending', 'running')`, id)
	return err
}

// ResetStale resets tasks stuck in 'running' state back to 'pending' (crash recovery).
func (s *AgentTaskStore) ResetStale(ctx context.Context, staleAfter time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'pending'
		WHERE status = 'running' AND last_run_at < $1`,
		time.Now().Add(-staleAfter))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ResetForNextRun resets a completed recurring task back to pending for the next schedule.
func (s *AgentTaskStore) ResetForNextRun(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'pending', iteration = 0, progress = '{}',
		    result = NULL, error_message = NULL, completed_at = NULL
		WHERE id = $1`, id)
	return err
}

// Create inserts a new task and returns it with the generated ID.
//
// Strategy defaults to "recurring" so existing call sites that only set
// Handler+Schedule keep their semantics. New callers explicitly set
// Strategy to one of {direct, structured, delegate} along with the
// matching fields (Plan / AcceptanceCriteria / DelegateTo / UseAgents).
func (s *AgentTaskStore) Create(ctx context.Context, task AgentTask) (AgentTask, error) {
	if task.ID == uuid.Nil {
		task.ID = uuid.New()
	}
	if task.Config == nil {
		task.Config = json.RawMessage(`{}`)
	}
	if task.Progress == nil {
		task.Progress = json.RawMessage(`{}`)
	}
	if task.Plan == nil {
		task.Plan = json.RawMessage(`{}`)
	}
	if task.Status == "" {
		task.Status = "pending"
	}
	if task.MaxIterations == 0 {
		task.MaxIterations = 10
	}
	if task.Strategy == "" {
		task.Strategy = StrategyRecurring
	}
	// pq.StringArray serialises nil as SQL NULL, which trips the
	// NOT-NULL constraint on tools/use_agents even though the schema
	// defaults to '{}'. The DEFAULT only kicks in when the column is
	// OMITTED, not when explicit NULL is supplied — so normalise to
	// an empty array here.
	if task.Tools == nil {
		task.Tools = pq.StringArray{}
	}
	if task.UseAgents == nil {
		task.UseAgents = pq.StringArray{}
	}
	if len(task.Config) == 0 {
		task.Config = json.RawMessage(`{}`)
	}
	if len(task.Plan) == 0 {
		task.Plan = json.RawMessage(`{}`)
	}
	if len(task.Progress) == 0 {
		task.Progress = json.RawMessage(`{}`)
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_tasks (soul_id, id, user_id, title, description, handler, config, tools,
		                         schedule, deadline, status, progress, max_iterations,
		                         strategy, delegate_to, plan, use_agents,
		                         acceptance_criteria, session_id, cadence)
		VALUES ($20::uuid,$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		task.ID, task.UserID, task.Title, task.Description,
		task.Handler, task.Config, task.Tools,
		task.Schedule, task.Deadline,
		task.Status, task.Progress, task.MaxIterations,
		task.Strategy, task.DelegateTo, task.Plan, task.UseAgents,
		task.AcceptanceCriteria, task.SessionID, task.Cadence,
		SoulIDFromContext(ctx))
	if err != nil {
		return AgentTask{}, fmt.Errorf("create agent task: %w", err)
	}
	return s.Get(ctx, task.ID)
}

// ToolOutputRecord is one row written into agent_task_tool_outputs by
// any tool that produced a bulky output worth auditing or replaying
// later. Generic by design: a single store covers research (browser_
// fetch HTML/PDF bodies), coding (reading source files), data
// analysis (db_query CSV blobs), etc. Per-tool semantics live in
// ToolInput / Metadata jsonb instead of typed columns so adding a new
// tool never requires a migration.
//
// The 500-char ToolTrace truncation in agent.Loop strips bulky output
// before downstream gates can see it. This store is the escape hatch.
type ToolOutputRecord struct {
	TaskID       uuid.UUID
	Iteration    int
	ToolName     string          // "browser_fetch", "<peer>_repo_read", ...
	ToolInput    json.RawMessage // raw tool input json
	Output       string          // the bulky body
	OutputFormat string          // "html" | "pdf" | "code" | "json" | "csv" | ...
	Metadata     json.RawMessage // per-tool typed extras
}

// RecordToolOutput appends a tool-output row for an agent_task iteration.
// Called from the tool handler closure immediately after the tool's
// own work succeeds — write happens once, store is append-only, no
// updates.
//
// Fire-and-forget from the caller's perspective: any error is the
// caller's to log; we don't want a transient DB hiccup to fail an
// otherwise-good tool call.
func (s *AgentTaskStore) RecordToolOutput(ctx context.Context, rec ToolOutputRecord) error {
	input := strings.TrimSpace(string(rec.ToolInput))
	if input == "" || input == "null" {
		input = "{}"
	}
	meta := strings.TrimSpace(string(rec.Metadata))
	if meta == "" || meta == "null" {
		meta = "{}"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_task_tool_outputs (
			soul_id, task_id, iteration, tool_name,
			tool_input, output, output_format, metadata
		) VALUES ($8::uuid, $1, $2, $3, $4::jsonb, $5, $6, $7::jsonb)`,
		rec.TaskID, rec.Iteration, rec.ToolName,
		input, rec.Output, rec.OutputFormat, meta,
		SoulIDFromContext(ctx))
	return err
}

// IterationRecord is the payload the scheduler hands AgentTaskStore.RecordIteration
// after every executeTask call. One row per iteration; never updated.
//
// Outcome is the scheduler's classification of what happened, not a raw
// IterationResult flag (Done can mean both "passed acceptance" and
// "rejected by acceptance", which are different outcomes for an
// operator reading the audit log).
type IterationRecord struct {
	TaskID           uuid.UUID
	Iteration        int
	StartedAt        time.Time
	CompletedAt      time.Time
	Outcome          string // "done" | "rejected" | "pause" | "continue" | "failed"
	IsFinal          bool
	AcceptanceMet    *bool  // nil = not evaluated this iteration
	AcceptanceReason string // evaluator's text when met=false
	Output           string
	Notify           string
	ToolCalls        json.RawMessage // jsonb [{name, input, output, error}, ...]
	Progress         json.RawMessage
	Error            string
	TraceID          string
	SpanID           string

	// Gate C (claim-level grounding) audit fields. Nil/empty when Gate C
	// didn't run this iteration (no acceptance criteria, no fetched
	// documents, LLM error, malformed verdict). Persisted into
	// agent_task_iterations so calibration queries can pick a threshold
	// from real data and so post-mortems on missed hallucinations have
	// the full claim breakdown to inspect.
	GroundedCount    *int
	UngroundedCount  *int
	GroundingVerdict json.RawMessage
}

// RecordIteration appends an audit row for one completed iteration of
// an agent_task. Fire-and-forget from the scheduler — never blocks the
// main path. Failures are logged but never propagated; the audit log
// is best-effort, not a correctness barrier.
func (s *AgentTaskStore) RecordIteration(ctx context.Context, rec IterationRecord) error {
	// Normalise tool_calls to a JSON array. Empty / nil / the literal
	// string "null" (which json.Marshal returns for a nil slice) would
	// all land as a scalar in the jsonb column and break downstream
	// jsonb_array_elements queries. Always coerce to "[]".
	tc := strings.TrimSpace(string(rec.ToolCalls))
	if tc == "" || tc == "null" {
		rec.ToolCalls = json.RawMessage("[]")
	}
	// Same for progress — empty bytes cast to jsonb raise 22P02
	// (invalid_text_representation). Default to an empty object.
	pg := strings.TrimSpace(string(rec.Progress))
	if pg == "" {
		rec.Progress = json.RawMessage("{}")
	}
	var accMet any
	if rec.AcceptanceMet != nil {
		accMet = *rec.AcceptanceMet
	}
	// nil-aware coercion: writing a typed *int that's nil through
	// database/sql ends up as 0 instead of NULL because the int gets
	// dereferenced lossily. Stash into `any` so a nil pointer reaches
	// the driver as untyped nil → NULL.
	var grounded, ungrounded any
	if rec.GroundedCount != nil {
		grounded = *rec.GroundedCount
	}
	if rec.UngroundedCount != nil {
		ungrounded = *rec.UngroundedCount
	}
	var groundingVerdict any
	if len(rec.GroundingVerdict) > 0 {
		groundingVerdict = string(rec.GroundingVerdict)
	}
	durationMs := int(rec.CompletedAt.Sub(rec.StartedAt).Milliseconds())
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_task_iterations (
			soul_id, task_id, iteration, started_at, completed_at, duration_ms,
			outcome, is_final, acceptance_met, acceptance_reason,
			output, notify, tool_calls, progress, error,
			trace_id, span_id,
			grounded_count, ungrounded_count, grounding_verdict
		) VALUES ($20::uuid, $1, $2, $3, $4, $5,
		          $6, $7, $8, $9,
		          $10, $11, $12::jsonb, $13::jsonb, $14,
		          $15, $16,
		          $17, $18, $19::jsonb)
		ON CONFLICT (task_id, iteration, started_at) DO NOTHING`,
		rec.TaskID, rec.Iteration, rec.StartedAt, rec.CompletedAt, durationMs,
		rec.Outcome, rec.IsFinal, accMet, rec.AcceptanceReason,
		rec.Output, rec.Notify, string(rec.ToolCalls), string(rec.Progress), rec.Error,
		rec.TraceID, rec.SpanID,
		grounded, ungrounded, groundingVerdict,
		SoulIDFromContext(ctx),
	)
	return err
}

// Get fetches a task by ID.
func (s *AgentTaskStore) Get(ctx context.Context, id uuid.UUID) (AgentTask, error) {
	var task AgentTask
	err := s.db.GetContext(ctx, &task, `SELECT * FROM agent_tasks WHERE id = $1`, id)
	return task, err
}

// GetByPrefix fetches a task whose UUID starts with the given hex prefix.
// Returns sql.ErrNoRows when no task matches and an error when multiple match.
func (s *AgentTaskStore) GetByPrefix(ctx context.Context, prefix string) (AgentTask, error) {
	var tasks []AgentTask
	err := s.db.SelectContext(ctx, &tasks,
		`SELECT * FROM agent_tasks WHERE id::text LIKE $1 ORDER BY created_at DESC LIMIT 2`,
		prefix+"%")
	if err != nil {
		return AgentTask{}, err
	}
	if len(tasks) == 0 {
		return AgentTask{}, fmt.Errorf("no task with prefix %q", prefix)
	}
	if len(tasks) > 1 {
		return AgentTask{}, fmt.Errorf("prefix %q is ambiguous (%d tasks match)", prefix, len(tasks))
	}
	return tasks[0], nil
}

// Resolve looks up a task by full UUID or by short prefix (≥8 hex chars).
func (s *AgentTaskStore) Resolve(ctx context.Context, raw string) (AgentTask, error) {
	if id, err := uuid.Parse(raw); err == nil {
		return s.Get(ctx, id)
	}
	if len(raw) < 8 {
		return AgentTask{}, fmt.Errorf("task id too short (need ≥8 chars): %q", raw)
	}
	return s.GetByPrefix(ctx, raw)
}

// EnsureRecurring creates a recurring task if one doesn't exist for (user_id, handler).
// If one exists, updates the schedule. Uses the unique partial index from migration 014.
func (s *AgentTaskStore) EnsureRecurring(ctx context.Context, userID uuid.UUID, handler, schedule, title string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_tasks (soul_id, user_id, handler, schedule, title, status, max_iterations,
		                         config, progress, strategy, plan)
		VALUES ($5::uuid, $1, $2, $3, $4, 'pending', 1, '{}', '{}', 'recurring', '{}')
		ON CONFLICT (soul_id, user_id, handler) WHERE schedule IS NOT NULL AND status != 'failed'
		DO UPDATE SET schedule = EXCLUDED.schedule, title = EXCLUDED.title`,
		userID, handler, schedule, title, SoulIDFromContext(ctx))
	return err
}

// Approve resumes a paused task (used after manual review milestones).
func (s *AgentTaskStore) Approve(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'pending'
		WHERE id = $1 AND status = 'paused'`, id)
	return err
}

// PauseTask saves progress, increments iteration, and sets status to 'paused'.
// Used by handlers that need to wait for an external event (e.g. A2A callback).
func (s *AgentTaskStore) PauseTask(ctx context.Context, id uuid.UUID, progress json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET progress = $2, status = 'paused', iteration = iteration + 1
		WHERE id = $1`, id, progress)
	return err
}

// WakePausedByPeerTask finds a paused task waiting for the given peer task ID
// and sets it back to pending. Returns the task ID or sql.ErrNoRows if none found.
func (s *AgentTaskStore) WakePausedByPeerTask(ctx context.Context, peerTaskID string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.GetContext(ctx, &id, `
		UPDATE agent_tasks
		SET status = 'pending'
		WHERE status = 'paused'
		  AND progress->>'peer_task_id' = $1
		RETURNING id`, peerTaskID)
	return id, err
}

// WakeStalePaused resets paused tasks back to pending if they've been paused too long
// (safety net for lost callbacks). Returns the number of tasks woken.
func (s *AgentTaskStore) WakeStalePaused(ctx context.Context, staleAfter time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'pending'
		WHERE status = 'paused' AND last_run_at < $1`,
		time.Now().Add(-staleAfter))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListForUser returns tasks for a user, optionally filtered by status.
func (s *AgentTaskStore) ListForUser(ctx context.Context, userID uuid.UUID, status string) ([]AgentTask, error) {
	var tasks []AgentTask
	if status != "" {
		err := s.db.SelectContext(ctx, &tasks,
			`SELECT * FROM agent_tasks WHERE user_id = $1 AND status = $2 ORDER BY created_at DESC`, userID, status)
		return tasks, err
	}
	err := s.db.SelectContext(ctx, &tasks,
		`SELECT * FROM agent_tasks WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	return tasks, err
}
