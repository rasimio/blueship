package mcp

import (
	"context"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// ServerRow is one configured MCP server — a vaelum.mcp_servers row.
type ServerRow struct {
	ID           uuid.UUID
	SoulID       uuid.UUID
	Name         string // label; the mcp__<label>__ namespace segment
	Transport    string // "http" | "stdio"
	URL          string
	Command      string
	Args         []string
	CredentialID *uuid.UUID
}

// CredentialFetcher resolves a credential id to a plaintext secret. The
// the host supplies it — the daemon never sees ciphertext or keys.
type CredentialFetcher func(ctx context.Context, credentialID uuid.UUID) (secret string, err error)

// store reads vaelum.mcp_servers and writes server status plus the mcp
// rows of vaelum.tool_catalog.
type store struct {
	db        *sqlx.DB
	credFetch CredentialFetcher
}

func (s *store) serversForSoul(ctx context.Context, soulID uuid.UUID) ([]ServerRow, error) {
	var rows []struct {
		ID           uuid.UUID      `db:"id"`
		SoulID       uuid.UUID      `db:"soul_id"`
		Name         string         `db:"name"`
		Transport    string         `db:"transport"`
		URL          *string        `db:"url"`
		Command      *string        `db:"command"`
		Args         pq.StringArray `db:"args"`
		CredentialID *uuid.UUID     `db:"credential_id"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT id, soul_id, name, transport, url, command, args, credential_id
		 FROM vaelum.mcp_servers
		 WHERE soul_id = $1 AND enabled = true`, soulID); err != nil {
		return nil, err
	}
	out := make([]ServerRow, 0, len(rows))
	for _, r := range rows {
		sr := ServerRow{
			ID: r.ID, SoulID: r.SoulID, Name: r.Name,
			Transport: r.Transport, Args: []string(r.Args), CredentialID: r.CredentialID,
		}
		if r.URL != nil {
			sr.URL = *r.URL
		}
		if r.Command != nil {
			sr.Command = *r.Command
		}
		out = append(out, sr)
	}
	return out, nil
}

// serversSignature returns a cheap fingerprint of a soul's enabled MCP
// servers. The Manager compares it each turn so a cabinet add/remove/edit
// is picked up on the next message rather than waiting out the cache TTL.
//
// MUST include only user-config fields (id / transport / url / command /
// args / credential), not bookkeeping columns like updated_at — markSynced
// bumps updated_at on every successful connect, so including it here used
// to flip the signature on every turn and trigger a cold reconnect on the
// next ToolsForSoul call (~1 s of needless reflex-time latency).
func (s *store) serversSignature(ctx context.Context, soulID uuid.UUID) string {
	var sig string
	_ = s.db.GetContext(ctx, &sig,
		`SELECT count(*)::text || ':' || coalesce(
		     md5(string_agg(
		         id::text || '|' || transport || '|' ||
		         coalesce(url, '') || '|' || coalesce(command, '') || '|' ||
		         coalesce(array_to_string(args, ','), '') || '|' ||
		         coalesce(credential_id::text, ''),
		         '~' ORDER BY id
		     )), '')
		 FROM vaelum.mcp_servers WHERE soul_id = $1 AND enabled = true`, soulID)
	return sig
}

func (s *store) markSynced(ctx context.Context, serverID uuid.UUID, toolCount int) {
	_, _ = s.db.ExecContext(ctx,
		`UPDATE vaelum.mcp_servers
		 SET status = 'connected', last_error = NULL, last_synced_at = now(),
		     tool_count = $2, updated_at = now()
		 WHERE id = $1`, serverID, toolCount)
}

func (s *store) markError(ctx context.Context, serverID uuid.UUID, msg string) {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, _ = s.db.ExecContext(ctx,
		`UPDATE vaelum.mcp_servers
		 SET status = 'error', last_error = $2, updated_at = now()
		 WHERE id = $1`, serverID, msg)
}

// upsertCatalogTools replaces a server's mcp rows in vaelum.tool_catalog
// with its freshly discovered tools, in one transaction. category is the
// server label so the cabinet groups a server's tools together.
func (s *store) upsertCatalogTools(ctx context.Context, serverID uuid.UUID, label string, defs []ToolDef) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM vaelum.tool_catalog WHERE kind = 'mcp' AND mcp_server_id = $1`,
		serverID); err != nil {
		return
	}
	for _, d := range defs {
		var schema any
		if len(d.InputSchema) > 0 {
			schema = string(d.InputSchema)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO vaelum.tool_catalog
			   (name, display_name, description, category, kind, core,
			    mcp_server_id, input_schema, default_enabled)
			 VALUES ($1, $2, $3, $4, 'mcp', false, $5, $6::jsonb, true)`,
			namespacedName(label, d.Name), d.Name, d.Description, label, serverID, schema); err != nil {
			return
		}
	}
	_ = tx.Commit()
}
