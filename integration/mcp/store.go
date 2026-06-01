package mcp

import (
	"context"

	"github.com/google/uuid"
)

// ServerRow is one configured MCP server.
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

// CredentialFetcher resolves a credential id to a plaintext secret. The host
// supplies it — the framework never sees ciphertext or keys.
type CredentialFetcher func(ctx context.Context, credentialID uuid.UUID) (secret string, err error)

// ServerStore is the host-supplied persistence seam for MCP servers and their
// discovered tools. The framework owns no platform schema; the host implements
// these against its own tables (e.g. a web platform's mcp_servers +
// tool_catalog). UpsertCatalogTools may use NamespacedName to build the
// registry tool name. Status mutations (MarkSynced/MarkError) are best-effort.
type ServerStore interface {
	ServersForSoul(ctx context.Context, soulID uuid.UUID) ([]ServerRow, error)
	ServersSignature(ctx context.Context, soulID uuid.UUID) string
	MarkSynced(ctx context.Context, serverID uuid.UUID, toolCount int)
	MarkError(ctx context.Context, serverID uuid.UUID, msg string)
	UpsertCatalogTools(ctx context.Context, serverID uuid.UUID, label string, defs []ToolDef)
}
