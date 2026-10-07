package session

import "context"

// ArchiveTerminalTaskSessions reconciles graph-step sessions after completion,
// cancellation or a crash between finalization and session cleanup. Evidence
// and messages stay intact; only the active marker changes.
func (s *Store) ArchiveTerminalTaskSessions(ctx context.Context, limit int) (int64, error) {
	limit = max(1, min(100, limit))
	result, err := s.db.ExecContext(ctx, `WITH finished AS (
 SELECT cs.id FROM chat_sessions cs JOIN agent_tasks t ON t.id=cs.source_id
 AND t.user_id=cs.user_id AND t.soul_id=cs.soul_id
 WHERE cs.active AND cs.source='agent_task' AND t.executor_version=2
 AND t.schedule IS NULL AND t.cadence IS NULL AND t.strategy <> 'recurring'
 AND t.status IN ('done','failed','canceled')
 ORDER BY cs.updated_at,cs.id LIMIT $1 FOR UPDATE OF cs SKIP LOCKED
 ) UPDATE chat_sessions cs SET active=false,updated_at=now()
 FROM finished f WHERE cs.id=f.id`, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
