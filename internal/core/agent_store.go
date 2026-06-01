package core

import (
	"context"
	"encoding/json"
	"fmt"
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

// PendingTasks returns tasks ready to be picked up by the scheduler.
func (s *AgentTaskStore) PendingTasks(ctx context.Context) ([]AgentTask, error) {
	var tasks []AgentTask
	err := s.db.SelectContext(ctx, &tasks, `
		SELECT * FROM agent_tasks
		WHERE status = 'pending'
		  AND (max_iterations = 0 OR iteration < max_iterations)
		  AND (deadline IS NULL OR deadline > NOW())
		ORDER BY created_at`)
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
func (s *AgentTaskStore) CompleteExhausted(ctx context.Context) {
	s.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'failed',
		    completed_at = NOW(),
		    error_message = COALESCE(error_message,
		        'max_iterations reached without satisfying acceptance criteria')
		WHERE status = 'pending'
		  AND schedule IS NULL
		  AND max_iterations > 0
		  AND iteration >= max_iterations`)
}

// SetPending resets a task back to pending (for retry after transient errors).
func (s *AgentTaskStore) SetPending(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks SET status = 'pending' WHERE id = $1`, id)
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
