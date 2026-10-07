package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type TaskArtifact struct {
	TaskID    uuid.UUID `db:"task_id" json:"task_id"`
	Version   int       `db:"version" json:"version"`
	Outcome   string    `db:"outcome" json:"outcome"`
	Body      string    `db:"body" json:"body"`
	Reason    string    `db:"reason" json:"reason,omitempty"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type TaskFinalization struct {
	Outcome string
	Body    string
	Reason  string
	// EmptyBody is the host's localized explanation if no draft exists.
	UnverifiedDraftFmt string
	EmptyBody          string
	Notify             string
	EmptyNotify        string
	Progress           json.RawMessage
	PendingDeliveries  []TaskDeliveryRef
	// ClaimStartedAt fences a worker; nil is for maintenance/cancellation.
	ClaimStartedAt *time.Time
	// ExpiredAt requires that the persisted deadline actually elapsed.
	ExpiredAt *time.Time
}

// FinalizeTask commits the result, terminal state and delivery intent in one
// transaction. No transport or LLM call runs inside it. A crash after commit
// leaves a retryable notification and a readable artifact, not lost work.
func (s *AgentTaskStore) FinalizeTask(ctx context.Context, id uuid.UUID, in TaskFinalization) (TaskArtifact, bool, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return TaskArtifact{}, false, err
	}
	defer tx.Rollback()
	artifact, changed, err := finalizeTaskTx(ctx, tx, id, in)
	if err != nil {
		return artifact, false, err
	}
	if err := tx.Commit(); err != nil {
		return artifact, false, err
	}
	return artifact, changed, nil
}

// finalizeTaskTx lets scoped controls fence ownership and finalize atomically.
func finalizeTaskTx(ctx context.Context, tx *sqlx.Tx, id uuid.UUID, in TaskFinalization) (TaskArtifact, bool, error) {
	var artifact TaskArtifact
	switch in.Outcome {
	case "completed", "partial", "blocked", "cancelled":
	default:
		return artifact, false, fmt.Errorf("invalid task outcome %q", in.Outcome)
	}
	var task AgentTask
	if err := tx.GetContext(ctx, &task, `SELECT * FROM agent_tasks WHERE id=$1 FOR UPDATE`, id); err != nil {
		return artifact, false, err
	}
	if task.Status != "pending" && task.Status != "running" && task.Status != "paused" {
		return artifact, false, nil
	}
	if task.ExecutorVersion == 2 && in.ExpiredAt == nil && in.Outcome != "cancelled" {
		if err := requireTaskRun(ctx, tx, id); err != nil {
			return artifact, false, err
		}
	}
	if in.ClaimStartedAt != nil && (task.Status != "running" || task.LastRunAt == nil || !task.LastRunAt.Equal(*in.ClaimStartedAt)) {
		return artifact, false, ErrTaskClaimLost
	}
	if in.ExpiredAt != nil && (task.Deadline == nil || task.Deadline.After(*in.ExpiredAt)) {
		return artifact, false, nil
	}
	if in.Outcome == "completed" && task.Deadline != nil && !task.Deadline.After(time.Now()) {
		return artifact, false, ErrTaskClaimLost
	}
	body := cleanTaskDraft(in.Body)
	if body == "" && in.Outcome == "completed" {
		return artifact, false, fmt.Errorf("completed task has no result")
	}

	if body == "" && task.ExecutorVersion == 2 {
		var progress struct {
			Final struct {
				ResponseChecked bool   `json:"response_checked"`
				SafeBody        string `json:"safe_body"`
				LastCheckedBody string `json:"last_checked_body"`
			} `json:"final_validation"`
		}
		// Final repairs live in task progress, while the finalize step retains
		// its original draft. Read the latest checked candidate under this task
		// lock before falling back to that older draft. ResponseChecked means
		// the output guard ran, not that acceptance/grounding passed: preserve
		// the caller's partial/cancelled outcome and never publish RepairBody.
		if json.Unmarshal(task.Progress, &progress) == nil {
			if progress.Final.ResponseChecked {
				body = cleanTaskDraft(progress.Final.SafeBody)
			}
			if body == "" {
				// Written only after a successful output guard, and retained while
				// a newer repair is still unchecked or its guard is unavailable.
				body = cleanTaskDraft(progress.Final.LastCheckedBody)
			}
		}
	}
	if body == "" && task.ExecutorVersion == 2 {
		var candidate string
		err := tx.GetContext(ctx, &candidate, `SELECT checkpoint->>'output' FROM agent_task_steps WHERE task_id=$1 AND kind='finalize' AND checkpoint->>'candidate_ready'='true' AND btrim(COALESCE(checkpoint->>'output',''))<>'' ORDER BY step_id LIMIT 1`, id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return artifact, false, err
		}
		body = cleanTaskDraft(candidate)
	}
	graphReadFallback := false
	if body == "" && task.ExecutorVersion == 2 {
		var draft string
		err := tx.GetContext(ctx, &draft, `SELECT COALESCE(string_agg(goal || E'\n' || result,E'\n\n' ORDER BY step_id),'') FROM agent_task_steps WHERE task_id=$1 AND status='done' AND btrim(result)<>''`, id)
		if err != nil {
			return artifact, false, err
		}
		body = cleanTaskDraft(draft)
		graphReadFallback = true
	}
	if (body == "" || graphReadFallback) && task.ExecutorVersion == 2 {
		var drafts []struct {
			Goal   string `db:"goal"`
			Output string `db:"output"`
			Review string `db:"review"`
		}
		// Only complete read candidates are publishable drafts. In-flight text
		// may be internal narration; action candidates are not success receipts.
		err := tx.SelectContext(ctx, &drafts, `SELECT goal,
		 CASE WHEN checkpoint->>'candidate_ready'='true' THEN checkpoint->>'output' ELSE checkpoint->>'last_candidate' END AS output,
		 CASE WHEN checkpoint->>'candidate_ready'='true' THEN COALESCE(checkpoint->>'review_reason',last_error,'') ELSE COALESCE(checkpoint->>'last_candidate_review',last_error,'') END AS review
		 FROM agent_task_steps WHERE task_id=$1 AND kind='read' AND status<>'done'
		 AND btrim(COALESCE(CASE WHEN checkpoint->>'candidate_ready'='true' THEN checkpoint->>'output' ELSE checkpoint->>'last_candidate' END,''))<>'' ORDER BY step_id`, id)
		if err != nil {
			return artifact, false, err
		}
		ui := UIStrings{UnverifiedDraftFmt: in.UnverifiedDraftFmt}
		ui.applyDefaults()
		var parts []string
		if body != "" {
			parts = append(parts, body)
		}
		for _, draft := range drafts {
			if text := cleanTaskDraft(draft.Output); text != "" {
				parts = append(parts, fmt.Sprintf(ui.UnverifiedDraftFmt, draft.Goal, text, draft.Review))
			}
		}
		body = strings.Join(parts, "\n\n")
	}
	if body == "" {
		var draft string
		err := tx.GetContext(ctx, &draft, `SELECT output FROM agent_task_submissions WHERE task_id=$1`, id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return artifact, false, err
		}
		body = cleanTaskDraft(draft)
	}
	if body == "" {
		var draft string
		err := tx.GetContext(ctx, &draft, `SELECT output FROM agent_task_checkpoints WHERE task_id=$1 AND btrim(output) <> ''`, id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return artifact, false, err
		}
		body = cleanTaskDraft(draft)
	}
	if body == "" {
		var draft string
		err := tx.GetContext(ctx, &draft, `SELECT output FROM agent_task_iterations WHERE task_id=$1 AND btrim(COALESCE(output,'')) <> '' ORDER BY iteration DESC LIMIT 1`, id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return artifact, false, err
		}
		body = cleanTaskDraft(draft)
	}
	if body == "" {
		body = strings.TrimSpace(in.EmptyBody)
		in.Notify = in.EmptyNotify
		if in.Outcome == "partial" {
			in.Outcome = "blocked"
		}
	}
	if body == "" {
		return artifact, false, fmt.Errorf("task finalization has no result or blocker explanation")
	}
	if err := tx.GetContext(ctx, &artifact, `INSERT INTO agent_task_artifacts (task_id,version,outcome,body,reason)
		VALUES ($1,COALESCE((SELECT max(version)+1 FROM agent_task_artifacts WHERE task_id=$1),1),$2,$3,$4)
		RETURNING *`, id, in.Outcome, body, in.Reason); err != nil {
		return artifact, false, err
	}
	status := TaskTerminalStatus(task, in.Outcome)
	errorMessage := in.Reason
	if status == "done" {
		errorMessage = "" // limitations live in the report, not a failure code
	}
	metadata, _ := json.Marshal(map[string]any{"phase": "terminal", "result_outcome": in.Outcome, "result_version": artifact.Version, "termination_reason": in.Reason})
	if _, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET status=$2, result=$3, error_message=NULLIF($4,''), completed_at=now(),
		progress=COALESCE(progress,'{}'::jsonb) || $5::jsonb || $6::jsonb WHERE id=$1`, id, status, body, errorMessage, jsonbObject(in.Progress), metadata); err != nil {
		return artifact, false, err
	}
	// A completed deliverable must not depend on an optional model-authored
	// notification. Queue the verified body when the handler omitted it.
	if in.Outcome == "completed" && strings.TrimSpace(in.Notify) == "" {
		in.Notify = body
	}
	if strings.TrimSpace(in.Notify) != "" {
		ref := TaskDeliveryRef{InputID: "task_result", ItemKey: fmt.Sprintf("version:%d", artifact.Version)}
		refs, err := normalizeTaskDeliveryRefs(append(append([]TaskDeliveryRef(nil), in.PendingDeliveries...), ref))
		if err != nil {
			return artifact, false, err
		}
		sortTaskDeliveryRefs(refs)
		attemptID := uuid.New()
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_task_notification_attempts
			(id,task_id,user_id,occurrence_key,message_text,state,error_message,attempt_count,next_attempt_at)
			VALUES ($1,$2,$3,$4,$5,'retryable','delivery_pending',0,now())`,
			attemptID, id, task.UserID, taskNotificationOccurrenceKey(refs), in.Notify); err != nil {
			return artifact, false, err
		}
		for _, ref := range refs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_task_notification_attempt_items (attempt_id,task_id,input_id,item_key) VALUES ($1,$2,$3,$4)`, attemptID, id, ref.InputID, ref.ItemKey); err != nil {
				return artifact, false, err
			}
		}
	}
	return artifact, true, nil
}

// Hidden planning/audit blocks are not a deliverable, including when only an
// interrupted prefix of a block survived. Keep ordinary report text intact.
var taskDraftInternals = regexp.MustCompile(`(?is)<scratchpad>.*?(?:</scratchpad>|$)|<<<(?:PLAN_JSON|PLAN_PATCH_JSON|EVIDENCE_JSON)\s.*?(?:>>>|$)`)

func cleanTaskDraft(body string) string {
	body = taskDraftInternals.ReplaceAllString(body, "")
	for _, marker := range []string{"[DONE]", "[CONTINUE]", "[PAUSE]", "[MILESTONE]", "[NOTIFY]", "[no-op]"} {
		body = strings.ReplaceAll(body, marker, "")
	}
	return strings.TrimSpace(body)
}

// TaskTerminalStatus maps a finalized outcome to the task row status. A
// one-shot graph task always ends with its report, which states its own
// limitations: a partial outcome there is a finished task, not a failure.
func TaskTerminalStatus(task AgentTask, outcome string) string {
	switch {
	case outcome == "completed", outcome == "partial" && TaskRunsToCompletion(task):
		return "done"
	case outcome == "cancelled":
		return "canceled"
	}
	return "failed"
}
