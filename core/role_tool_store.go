package core

import (
	"context"
	"sync"

	"github.com/jmoiron/sqlx"
)

// RoleToolStore caches role→tool_name mappings from the role_tools table.
// Roles with no rows in the table are not stored — callers should treat
// a nil/missing entry as "use all tools" (backwards-compatible for cloud models).
type RoleToolStore struct {
	db    *sqlx.DB
	mu    sync.RWMutex
	roles map[string][]string // role → ordered tool names
}

// NewRoleToolStore creates a new store backed by the given DB connection.
func NewRoleToolStore(db *sqlx.DB) *RoleToolStore {
	return &RoleToolStore{db: db, roles: make(map[string][]string)}
}

// Load reads all role_tools rows from DB. Call once at startup.
func (s *RoleToolStore) Load(ctx context.Context) error {
	rows, err := s.db.QueryxContext(ctx, `SELECT role, tool_name FROM role_tools ORDER BY sort_order, tool_name`)
	if err != nil {
		return err
	}
	defer rows.Close()

	m := make(map[string][]string)
	for rows.Next() {
		var role, name string
		if err := rows.Scan(&role, &name); err != nil {
			return err
		}
		m[role] = append(m[role], name)
	}

	s.mu.Lock()
	s.roles = m
	s.mu.Unlock()
	return nil
}

// Get returns the ordered tool names for a role, or nil if the role has no
// entries (meaning "use all tools").
func (s *RoleToolStore) Get(role string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.roles[role]
}

// Refresh reloads from DB. Safe to call from hot path (e.g. /reset).
func (s *RoleToolStore) Refresh(ctx context.Context) error {
	return s.Load(ctx)
}
