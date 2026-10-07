package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// TaskStatus exposes task state without execution configuration or tool payloads.
// Unknown progress and ETA remain nil until measured step data is available.
type TaskStatus struct {
	WorkerHeartbeatAt     *time.Time `json:"worker_heartbeat_at,omitempty"`
	LastProgressAt        *time.Time `json:"last_progress_at,omitempty"`
	DeliveryState         string     `json:"delivery_state,omitempty"`
	DeliveryNextAttemptAt *time.Time `json:"delivery_next_attempt_at,omitempty"`
	DeliveryConfirmedAt   *time.Time `json:"delivery_confirmed_at,omitempty"`
	NextAttemptAt         *time.Time `json:"next_attempt_at,omitempty"`
	CompletedSteps        int        `json:"completed_steps"`
	TotalSteps            int        `json:"total_steps"`
	ID                    uuid.UUID  `json:"id"`
	Title                 string     `json:"title"`
	Stage                 string     `json:"stage"`
	Outcome               string     `json:"outcome,omitempty"`
	Result                *string    `json:"result,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	CompletedAt           *time.Time `json:"completed_at,omitempty"`
	ProgressPercent       *int       `json:"progress_percent,omitempty"`
	ETAUpperSeconds       *int       `json:"eta_upper_seconds,omitempty"`
	ETASeconds            *int       `json:"eta_seconds,omitempty"`
}

type TaskStatusReader struct {
	db     *sqlx.DB
	schema string
}

func NewTaskStatusReader(db *sqlx.DB) *TaskStatusReader { return &TaskStatusReader{db: db} }

// NewTaskStatusReaderInSchema reads the canonical projection from an explicit
// schema without changing the shared connection pool's search_path.
func NewTaskStatusReaderInSchema(db *sqlx.DB, schema string) *TaskStatusReader {
	return &TaskStatusReader{db: db, schema: schema}
}

func (r *TaskStatusReader) qualify(query string) string {
	if r.schema == "" {
		return query
	}
	prefix := pq.QuoteIdentifier(r.schema) + "."
	return strings.NewReplacer("agent_tasks", prefix+"agent_tasks", "agent_task_runs", prefix+"agent_task_runs", "agent_task_steps", prefix+"agent_task_steps", "agent_task_artifacts", prefix+"agent_task_artifacts", "agent_task_notification_attempt_items", prefix+"agent_task_notification_attempt_items", "agent_task_notification_attempts", prefix+"agent_task_notification_attempts", "agent_task_dismissals", prefix+"agent_task_dismissals").Replace(query)
}

var taskStatusID = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})?$`)

// Read requires both owner and assistant, including for explicit identifiers.
// A short ID must resolve uniquely inside that scope. Lists omit report bodies.
func (r *TaskStatusReader) Read(ctx context.Context, userID, soulID uuid.UUID, id string) ([]TaskStatus, error) {
	return r.read(ctx, userID, soulID, id, 20, 0, false)
}

// ReadPage includes recent outcomes so a missed notification cannot hide a
// completed report. One extra row tells the caller whether another page exists.
func (r *TaskStatusReader) ReadPage(ctx context.Context, userID, soulID uuid.UUID, page int) ([]TaskStatus, error) {
	if page < 0 || page > 10000 {
		return nil, fmt.Errorf("invalid status page")
	}
	return r.read(ctx, userID, soulID, "", 6, page*5, true)
}

func (r *TaskStatusReader) read(ctx context.Context, userID, soulID uuid.UUID, id string, limit, offset int, recent bool) ([]TaskStatus, error) {
	if userID == uuid.Nil || soulID == uuid.Nil {
		return nil, sql.ErrNoRows
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if id != "" && !taskStatusID.MatchString(id) {
		return nil, fmt.Errorf("invalid task identifier")
	}
	var tasks []struct {
		AgentTask
		WorkerHeartbeatAt *time.Time `db:"worker_heartbeat_at"`
	}
	query := `SELECT agent_tasks.*,r.heartbeat_at AS worker_heartbeat_at FROM agent_tasks LEFT JOIN agent_task_runs r ON r.task_id=agent_tasks.id WHERE user_id=$1 AND soul_id=$2 AND schedule IS NULL AND cadence IS NULL AND strategy <> 'recurring'`
	args := []any{userID, soulID}
	if id != "" {
		query += ` AND id::text LIKE $3 ORDER BY created_at DESC LIMIT 2`
		args = append(args, id+"%")
	} else {
		// Owner-dismissed results leave lists only; lookup by ID still works.
		query += ` AND NOT EXISTS (SELECT 1 FROM agent_task_dismissals d WHERE d.task_id=agent_tasks.id)`
		if recent {
			query += ` AND (status IN ('pending','running','paused') OR completed_at >= NOW()-INTERVAL '24 hours') ORDER BY (status IN ('pending','running','paused')) DESC,created_at DESC,id DESC LIMIT $3 OFFSET $4`
		} else {
			query += ` AND status IN ('pending','running','paused') ORDER BY created_at DESC,id DESC LIMIT $3 OFFSET $4`
		}
		args = append(args, limit, offset)
	}
	if err := r.db.SelectContext(ctx, &tasks, r.qualify(query), args...); err != nil {
		return nil, err
	}
	if id != "" && len(tasks) != 1 {
		return nil, sql.ErrNoRows
	}
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID.String())
	}
	stepsByTask := map[uuid.UUID][]stepTiming{}
	if len(ids) > 0 {
		var rows []stepTiming
		err := r.db.SelectContext(ctx, &rows, r.qualify(`SELECT s.task_id,s.step_id,s.kind,s.status,s.dependencies,s.next_attempt_at,s.started_at,s.completed_at,s.progress_at,s.attempts,
   COALESCE(s.checkpoint->>'phase','') AS checkpoint_phase,
   h.samples,COALESCE(h.median_seconds,0) AS median_seconds,COALESCE(h.high_seconds,0) AS high_seconds,
   a.pool_samples,COALESCE(a.pool_median_seconds,0) AS pool_median_seconds,COALESCE(a.pool_high_seconds,0) AS pool_high_seconds
   FROM agent_task_steps s JOIN agent_tasks t ON t.id=s.task_id
   CROSS JOIN LATERAL (
    SELECT count(*) AS samples,percentile_cont(0.5) WITHIN GROUP (ORDER BY duration) AS median_seconds,
     percentile_cont(0.9) WITHIN GROUP (ORDER BY duration) AS high_seconds
    FROM (SELECT extract(epoch FROM (p.completed_at-p.started_at))::float8 AS duration
     FROM agent_task_steps p JOIN agent_tasks history ON history.id=p.task_id
     WHERE s.status<>'done'
      AND history.handler=t.handler AND history.executor_version=t.executor_version
      AND p.task_id<>s.task_id AND p.kind=s.kind
      AND p.status='done' AND p.attempts=1 AND p.completed_at>p.started_at
      AND p.completed_at>clock_timestamp()-interval '30 days'
     ORDER BY p.completed_at DESC LIMIT 100) observations
   ) h
   CROSS JOIN LATERAL (
    SELECT count(*) AS pool_samples,percentile_cont(0.5) WITHIN GROUP (ORDER BY duration) AS pool_median_seconds,
     percentile_cont(0.9) WITHIN GROUP (ORDER BY duration) AS pool_high_seconds
    FROM (SELECT extract(epoch FROM (p.completed_at-p.started_at))::float8 AS duration
     FROM agent_task_steps p JOIN agent_tasks history ON history.id=p.task_id
     WHERE s.status<>'done'
      AND history.handler=t.handler AND history.executor_version=t.executor_version
      AND p.task_id<>s.task_id
      AND p.status='done' AND p.attempts=1 AND p.completed_at>p.started_at
      AND p.completed_at>clock_timestamp()-interval '30 days'
     ORDER BY p.completed_at DESC LIMIT 100) observations
   ) a
   WHERE t.user_id=$1 AND t.soul_id=$2 AND s.task_id=ANY($3::uuid[])`), userID, soulID, pq.Array(ids))
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			stepsByTask[row.TaskID] = append(stepsByTask[row.TaskID], row)
		}
	}
	statuses := make([]TaskStatus, 0, len(tasks))
	deliveries, err := r.readResultDeliveries(ctx, userID, soulID, ids)
	if err != nil {
		return nil, err
	}
	var final struct {
		Samples int     `db:"samples"`
		Median  float64 `db:"median_seconds"`
		High    float64 `db:"high_seconds"`
	}
	finalLoaded := false
	for _, task := range tasks {
		status := projectTaskStatus(task.AgentTask)
		status.WorkerHeartbeatAt = task.WorkerHeartbeatAt
		if delivery, ok := deliveries[task.ID]; ok {
			status.DeliveryState = delivery.State
			status.DeliveryNextAttemptAt = delivery.NextAttemptAt
			status.DeliveryConfirmedAt = delivery.ConfirmedAt
		} else if status.Stage == "terminal" {
			status.DeliveryState = "unknown"
		}
		steps := stepsByTask[task.ID]
		status.Stage = projectGraphStage(status.Stage, steps)
		status.TotalSteps = len(steps)
		for _, step := range steps {
			for _, at := range []*time.Time{step.ProgressAt, step.CompletedAt} {
				if at != nil && (status.LastProgressAt == nil || at.After(*status.LastProgressAt)) {
					status.LastProgressAt = at
				}
			}
			if step.Status == "done" {
				status.CompletedSteps++
			}
		}
		now := time.Now()
		low, high := estimateTaskETA(status.Stage, steps, now)
		if low == nil && len(steps) > 0 && status.CompletedSteps == len(steps) {
			if !finalLoaded {
				finalLoaded = true
				if err := r.db.GetContext(ctx, &final, r.qualify(finalCheckHistorySQL)); err != nil {
					return nil, err
				}
			}
			var reportAt time.Time
			for _, step := range steps {
				if step.CompletedAt != nil && step.CompletedAt.After(reportAt) {
					reportAt = *step.CompletedAt
				}
			}
			low, high = estimateFinalCheck(status.Stage, now.Sub(reportAt).Seconds(), final.Samples, final.Median, final.High)
		}
		status.ETASeconds, status.ETAUpperSeconds = boundTaskETA(status.Stage, TaskStopDeadline(task.AgentTask), low, high, now)
		if status.TotalSteps > 0 && status.Stage != "terminal" {
			percentage := min(99, 100*status.CompletedSteps/status.TotalSteps)
			status.ProgressPercent = &percentage
		}
		if id == "" {
			status.Result = nil
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// finalCheckHistorySQL measures report verification: from the finalizer's
// completion to the task's completion, over recent graph tasks.
const finalCheckHistorySQL = `SELECT count(*) AS samples,
 COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY d),0) AS median_seconds,
 COALESCE(percentile_cont(0.9) WITHIN GROUP (ORDER BY d),0) AS high_seconds
 FROM (SELECT extract(epoch FROM (t.completed_at-s.completed_at))::float8 AS d
  FROM agent_tasks t JOIN agent_task_steps s ON s.task_id=t.id AND s.kind='finalize' AND s.status='done'
  WHERE t.executor_version=2 AND t.completed_at>s.completed_at AND t.completed_at>clock_timestamp()-interval '30 days'
  ORDER BY t.completed_at DESC LIMIT 50) checks`

func projectTaskStatus(task AgentTask) TaskStatus {
	s := TaskStatus{ID: task.ID, Title: task.Title, CreatedAt: task.CreatedAt, CompletedAt: task.CompletedAt}
	var progress struct {
		FinalValidation *struct {
			RepairPending bool `json:"repair_pending"`
		} `json:"final_validation"`
		NextAttemptAt *time.Time `json:"next_attempt_at"`
		Phase         string     `json:"phase"`
		Outcome       string     `json:"result_outcome"`
	}
	_ = json.Unmarshal(task.Progress, &progress)
	switch task.Status {
	case "done", "failed", "canceled":
		s.Stage = "terminal"
		s.Outcome = progress.Outcome
		if s.Outcome == "" {
			switch task.Status {
			case "done":
				s.Outcome = "completed"
			case "canceled":
				s.Outcome = "cancelled"
			default:
				s.Outcome = "blocked"
			}
		}
		s.Result = task.Result
	case "paused":
		s.Stage = "waiting"
	default:
		if task.Status == "running" {
			s.Stage = "running"
		} else {
			s.Stage = "queued"
		}
		switch progress.Phase {
		case "planning", "finalizing", "verification_wait", "retry_wait":
			s.Stage = progress.Phase
			s.NextAttemptAt = progress.NextAttemptAt
			if progress.Phase == "finalizing" && progress.FinalValidation != nil && !progress.FinalValidation.RepairPending {
				s.Stage = "verifying"
			}
		}
	}
	if s.Outcome == "completed" {
		n := 100
		s.ProgressPercent = &n
	}
	return s
}

// Refine an executing task from persisted worker checkpoints, without exposing
// source content. Mixed research/review stays running until all active workers
// are verifying; waiting/terminal states retain their explicit task projection.
func projectGraphStage(stage string, steps []stepTiming) string {
	if stage != "running" {
		return stage
	}
	active, checking := 0, 0
	finalizing := false
	for _, step := range steps {
		if step.Status != "running" {
			continue
		}
		active++
		if step.CheckpointPhase == "verification" {
			checking++
		} else if step.Kind == "finalize" {
			finalizing = true
		}
	}
	if active > 0 && checking == active {
		return "verifying"
	}
	if finalizing {
		return "finalizing"
	}
	return stage
}
