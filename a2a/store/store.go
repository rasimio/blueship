// Package store is the sqlx data-access layer for blueship's a2a_peers,
// a2a_remote_tools, a2a_calls, and a2a_events tables, plus a read-through
// helper for the unified `tools` table that supplies exposed-tool metadata
// for the agent card. The package is deliberately thin — it contains SQL
// only, no business logic. Server, client, and tracer layers compose it.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/rasimio/blueship/a2a"
)

// ErrNotFound is returned by Get/Read helpers when the row does not exist.
var ErrNotFound = errors.New("a2a.store: not found")

// Store wraps a sqlx pool bound to the ship's blueship schema. The caller
// is responsible for supplying a pool whose search_path already resolves
// a2a_* tables (i.e. the "ship" pool in bscore.Deps).
type Store struct {
	db *sqlx.DB
}

// New constructs a Store.
func New(db *sqlx.DB) *Store {
	return &Store{db: db}
}

// ---------------------------------------------------------------------------
// Peers
// ---------------------------------------------------------------------------

// UpsertPeer inserts or updates a known peer. Auth token and base_url are
// refreshed on conflict; agent_card is left untouched (call SaveAgentCard
// separately after a successful discovery).
func (s *Store) UpsertPeer(ctx context.Context, name, baseURL, token string) (*a2a.Peer, error) {
	const q = `
		INSERT INTO a2a_peers (name, base_url, auth_token, enabled)
		VALUES ($1, $2, $3, true)
		ON CONFLICT (name) DO UPDATE
		SET base_url   = EXCLUDED.base_url,
		    auth_token = EXCLUDED.auth_token,
		    updated_at = now()
		RETURNING id, name, base_url, auth_token, agent_card, card_fetched_at,
		          last_seen_at, enabled, created_at, updated_at
	`
	var p a2a.Peer
	if err := s.db.GetContext(ctx, &p, q, name, baseURL, token); err != nil {
		return nil, fmt.Errorf("UpsertPeer: %w", err)
	}
	return &p, nil
}

// GetPeerByName looks up a peer by its stable name.
func (s *Store) GetPeerByName(ctx context.Context, name string) (*a2a.Peer, error) {
	const q = `
		SELECT id, name, base_url, auth_token, agent_card, card_fetched_at,
		       last_seen_at, enabled, created_at, updated_at
		FROM a2a_peers
		WHERE name = $1
	`
	var p a2a.Peer
	if err := s.db.GetContext(ctx, &p, q, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("GetPeerByName: %w", err)
	}
	return &p, nil
}

// ListEnabledPeers returns every enabled peer row.
func (s *Store) ListEnabledPeers(ctx context.Context) ([]a2a.Peer, error) {
	const q = `
		SELECT id, name, base_url, auth_token, agent_card, card_fetched_at,
		       last_seen_at, enabled, created_at, updated_at
		FROM a2a_peers
		WHERE enabled = true
		ORDER BY name
	`
	var out []a2a.Peer
	if err := s.db.SelectContext(ctx, &out, q); err != nil {
		return nil, fmt.Errorf("ListEnabledPeers: %w", err)
	}
	return out, nil
}

// SaveAgentCard caches a peer's discovered /.well-known/agent payload.
func (s *Store) SaveAgentCard(ctx context.Context, peerID string, card json.RawMessage) error {
	const q = `
		UPDATE a2a_peers
		SET agent_card      = $1,
		    card_fetched_at = now(),
		    last_seen_at    = now(),
		    updated_at      = now()
		WHERE id = $2
	`
	if _, err := s.db.ExecContext(ctx, q, []byte(card), peerID); err != nil {
		return fmt.Errorf("SaveAgentCard: %w", err)
	}
	return nil
}

// (Exposed tools are served by the dispatcher from the in-memory
// ToolRegistry — see a2a/dispatcher.go. There is no DB-side exposed-
// tool table; the earlier ListExposedTools helper has been retired.)

// ---------------------------------------------------------------------------
// Remote tools (tools imported from peers)
// ---------------------------------------------------------------------------

// UpsertRemoteTool caches a peer-exposed tool that this ship plans to
// invoke. Called by the a2a client after successfully fetching an agent
// card at startup.
func (s *Store) UpsertRemoteTool(ctx context.Context, peerID string, t a2a.ExposedTool) error {
	const q = `
		INSERT INTO a2a_remote_tools (peer_id, name, mode, description, schema, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (peer_id, name) DO UPDATE
		SET mode         = EXCLUDED.mode,
		    description  = EXCLUDED.description,
		    schema       = EXCLUDED.schema,
		    last_seen_at = now()
	`
	_, err := s.db.ExecContext(ctx, q, peerID, t.Name, string(t.Mode), t.Description, []byte(t.Schema))
	if err != nil {
		return fmt.Errorf("UpsertRemoteTool: %w", err)
	}
	return nil
}

// ListRemoteTools returns every remote tool imported from the given peer.
func (s *Store) ListRemoteTools(ctx context.Context, peerID string) ([]a2a.RemoteTool, error) {
	const q = `
		SELECT p.name AS peer_name, rt.name, rt.mode, rt.description, rt.schema, rt.last_seen_at
		FROM a2a_remote_tools rt
		JOIN a2a_peers p ON p.id = rt.peer_id
		WHERE rt.peer_id = $1
		ORDER BY rt.name
	`
	type row struct {
		PeerName    string          `db:"peer_name"`
		Name        string          `db:"name"`
		Mode        string          `db:"mode"`
		Description string          `db:"description"`
		Schema      json.RawMessage `db:"schema"`
		LastSeenAt  time.Time       `db:"last_seen_at"`
	}
	var rows []row
	if err := s.db.SelectContext(ctx, &rows, q, peerID); err != nil {
		return nil, fmt.Errorf("ListRemoteTools: %w", err)
	}
	out := make([]a2a.RemoteTool, 0, len(rows))
	for _, r := range rows {
		out = append(out, a2a.RemoteTool{
			PeerName:    r.PeerName,
			Name:        r.Name,
			Mode:        a2a.ToolMode(r.Mode),
			Description: r.Description,
			Schema:      r.Schema,
			LastSeenAt:  r.LastSeenAt,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Calls (the audit trail)
// ---------------------------------------------------------------------------

// CreateCall inserts a new call row in pending state.
func (s *Store) CreateCall(ctx context.Context, c a2a.Call) (string, error) {
	if c.ID == "" {
		c.ID = uuid.NewString()
	}
	if c.State == "" {
		c.State = a2a.CallStatePending
	}
	const q = `
		INSERT INTO a2a_calls (id, peer_id, peer_name, direction, tool_name, mode,
		                       correlation_id, input, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	_, err := s.db.ExecContext(ctx, q,
		c.ID, c.PeerID, c.PeerName, string(c.Direction), c.ToolName, string(c.Mode),
		c.CorrelationID, []byte(c.Input), string(c.State))
	if err != nil {
		return "", fmt.Errorf("CreateCall: %w", err)
	}
	return c.ID, nil
}

// UpdateCallState transitions a call and optionally records output or error.
// completedAt is set to now() when moving to a terminal state.
func (s *Store) UpdateCallState(ctx context.Context, id string, state a2a.CallState, output json.RawMessage, callErr string) error {
	var completed *time.Time
	if state.IsTerminal() {
		t := time.Now()
		completed = &t
	}
	const q = `
		UPDATE a2a_calls
		SET state        = $1,
		    output       = COALESCE($2, output),
		    error        = COALESCE(NULLIF($3, ''), error),
		    completed_at = COALESCE($4, completed_at)
		WHERE id = $5
	`
	var outBytes any
	if len(output) > 0 {
		outBytes = []byte(output)
	}
	_, err := s.db.ExecContext(ctx, q, string(state), outBytes, callErr, completed, id)
	if err != nil {
		return fmt.Errorf("UpdateCallState: %w", err)
	}
	return nil
}

// GetCall reads one call by id.
func (s *Store) GetCall(ctx context.Context, id string) (*a2a.Call, error) {
	const q = `
		SELECT id, peer_id, peer_name, direction, tool_name, mode,
		       correlation_id, COALESCE(input,  '{}'::jsonb) AS input,
		       COALESCE(output, '{}'::jsonb) AS output,
		       error, state, created_at, completed_at
		FROM a2a_calls
		WHERE id = $1
	`
	var c a2a.Call
	if err := s.db.GetContext(ctx, &c, q, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("GetCall: %w", err)
	}
	return &c, nil
}

// ListCalls returns recent calls for audit / CLI inspection.
func (s *Store) ListCalls(ctx context.Context, peerName string, direction a2a.CallDirection, limit int) ([]a2a.Call, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	const qBase = `
		SELECT id, peer_id, peer_name, direction, tool_name, mode,
		       correlation_id, COALESCE(input,  '{}'::jsonb) AS input,
		       COALESCE(output, '{}'::jsonb) AS output,
		       error, state, created_at, completed_at
		FROM a2a_calls
	`
	where := ""
	args := []any{}
	if peerName != "" {
		args = append(args, peerName)
		where += fmt.Sprintf(" WHERE peer_name = $%d", len(args))
	}
	if direction != "" {
		args = append(args, string(direction))
		if where == "" {
			where += " WHERE "
		} else {
			where += " AND "
		}
		where += fmt.Sprintf("direction = $%d", len(args))
	}
	args = append(args, limit)
	q := qBase + where + fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))

	var out []a2a.Call
	if err := s.db.SelectContext(ctx, &out, q, args...); err != nil {
		return nil, fmt.Errorf("ListCalls: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// AppendEvent writes a new event row for the given call, auto-numbering
// seq as the max existing + 1. The SELECT and INSERT are a single
// statement to avoid duplicate-seq races under concurrent emitters.
func (s *Store) AppendEvent(ctx context.Context, callID string, eventType a2a.EventType, payload json.RawMessage, isFinal bool) (*a2a.Event, error) {
	const q = `
		WITH next AS (
			SELECT COALESCE(MAX(seq), 0) + 1 AS seq
			FROM a2a_events
			WHERE call_id = $1
		)
		INSERT INTO a2a_events (call_id, seq, type, payload, is_final)
		SELECT $1, next.seq, $2, $3, $4 FROM next
		RETURNING id, call_id, seq, type, COALESCE(payload, '{}'::jsonb) AS payload, is_final, created_at
	`
	var ev a2a.Event
	var payloadBytes any
	if len(payload) > 0 {
		payloadBytes = []byte(payload)
	}
	if err := s.db.GetContext(ctx, &ev, q, callID, string(eventType), payloadBytes, isFinal); err != nil {
		return nil, fmt.Errorf("AppendEvent: %w", err)
	}
	return &ev, nil
}

// EventsSince returns events for a call with seq > since, ordered. Used
// by the SSE handler on reconnect (Last-Event-ID) and by the client on
// catch-up before subscribing to the live channel.
func (s *Store) EventsSince(ctx context.Context, callID string, since int, limit int) ([]a2a.Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const q = `
		SELECT id, call_id, seq, type, COALESCE(payload, '{}'::jsonb) AS payload, is_final, created_at
		FROM a2a_events
		WHERE call_id = $1 AND seq > $2
		ORDER BY seq
		LIMIT $3
	`
	var out []a2a.Event
	if err := s.db.SelectContext(ctx, &out, q, callID, since, limit); err != nil {
		return nil, fmt.Errorf("EventsSince: %w", err)
	}
	return out, nil
}

// HasTerminalEvent reports whether the call has already reached an
// is_final=true row — used by the SSE handler to close idle connections.
func (s *Store) HasTerminalEvent(ctx context.Context, callID string) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM a2a_events WHERE call_id = $1 AND is_final = true)`
	var exists bool
	if err := s.db.GetContext(ctx, &exists, q, callID); err != nil {
		return false, fmt.Errorf("HasTerminalEvent: %w", err)
	}
	return exists, nil
}
