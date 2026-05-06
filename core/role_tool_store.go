package core

import (
	"context"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// RoleToolStore maps a role name (e.g. "cortex", "reflex", "background")
// to the ordered list of tool names that role is allowed to use.
//
// Source-of-truth lives in the `role_tools` table (migration 007).
// Ship.Run loads it at boot via LoadRoleToolStore. Tunable per role
// without redeploy. Roles without a row default to "no allowlist" —
// handlers that need a stricter set must add a row.
//
// NewRoleToolStore (in-memory map) is kept for tests and callers that
// build allowlists ad-hoc; production wiring goes through
// LoadRoleToolStore.
type RoleToolStore struct {
	roles map[string][]string
}

// NewRoleToolStore wraps an in-memory map. Used by tests and tooling;
// production loads from DB via LoadRoleToolStore.
func NewRoleToolStore(roles map[string][]string) *RoleToolStore {
	if roles == nil {
		roles = map[string][]string{}
	}
	return &RoleToolStore{roles: roles}
}

// LoadRoleToolStore reads role→tools rows from the `role_tools` table
// (search_path includes blueship). Returns an empty store on error so
// callers can degrade gracefully (every tool available); the error is
// returned alongside for logging.
func LoadRoleToolStore(ctx context.Context, db *sqlx.DB) (*RoleToolStore, error) {
	rows, err := db.QueryxContext(ctx, `SELECT role, tools FROM role_tools`)
	if err != nil {
		return NewRoleToolStore(nil), err
	}
	defer rows.Close()
	roles := map[string][]string{}
	for rows.Next() {
		var role string
		var tools pq.StringArray
		if err := rows.Scan(&role, &tools); err != nil {
			return NewRoleToolStore(roles), err
		}
		roles[role] = []string(tools)
	}
	return &RoleToolStore{roles: roles}, nil
}

// Get returns the ordered tool names for a role, or nil when the role
// has no entry (meaning "no allowlist — handler decides").
func (s *RoleToolStore) Get(role string) []string {
	if s == nil {
		return nil
	}
	return s.roles[role]
}
