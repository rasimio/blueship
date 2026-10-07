package core

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// TaskSourceObservation is runtime metadata, not a date extracted from a page.
// Replays do not create a new observation and are excluded from this projection.
type TaskSourceObservation struct {
	URLs       []string
	ObservedAt time.Time
}

func (s *AgentTaskStore) TaskSourceObservations(ctx context.Context, taskID, userID, soulID uuid.UUID) ([]TaskSourceObservation, error) {
	if taskID == uuid.Nil || userID == uuid.Nil || soulID == uuid.Nil {
		return nil, sql.ErrNoRows
	}
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(o.tool_input->>'url',''),COALESCE(o.metadata->>'requested_url',''),COALESCE(o.metadata->>'final_url',''),COALESCE(o.metadata->>'observed_at',''),o.created_at
 FROM agent_task_tool_outputs o JOIN agent_tasks t ON t.id=o.task_id
 WHERE t.id=$1 AND t.user_id=$2 AND t.soul_id=$3 AND o.soul_id=t.soul_id AND o.tool_name='browser_fetch'
 AND o.output<>'' AND (o.metadata->>'from_cache') IS DISTINCT FROM 'true'`, taskID, userID, soulID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var observations []TaskSourceObservation
	for rows.Next() {
		var input, requested, final, stamp string
		var at time.Time
		if err := rows.Scan(&input, &requested, &final, &stamp, &at); err != nil {
			return nil, err
		}
		if stamp != "" {
			at, err = time.Parse(time.RFC3339Nano, stamp)
			if err != nil {
				return nil, fmt.Errorf("invalid saved source observation time")
			}
		}
		observations = append(observations, TaskSourceObservation{URLs: []string{input, requested, final}, ObservedAt: at.UTC()})
	}
	return observations, rows.Err()
}
