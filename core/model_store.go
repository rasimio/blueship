package core

import (
	"context"
	"sync"

	"github.com/jmoiron/sqlx"
)

// ModelConfigStore reads model assignments from the model_config table.
// Thread-safe; caches in memory, refreshed on demand (e.g. /reset).
type ModelConfigStore struct {
	db    *sqlx.DB
	mu    sync.RWMutex
	cache map[string]ModelRef // role → ModelRef
}

// NewModelConfigStore creates a store and loads the initial config.
func NewModelConfigStore(db *sqlx.DB) *ModelConfigStore {
	return &ModelConfigStore{
		db:    db,
		cache: make(map[string]ModelRef),
	}
}

// Load reads all model assignments from DB into cache.
func (s *ModelConfigStore) Load(ctx context.Context) error {
	rows, err := s.db.QueryxContext(ctx, "SELECT role, provider, model_name FROM model_config")
	if err != nil {
		return err
	}
	defer rows.Close()

	m := make(map[string]ModelRef)
	for rows.Next() {
		var role, provider, modelName string
		if err := rows.Scan(&role, &provider, &modelName); err != nil {
			return err
		}
		m[role] = ModelRef{Provider: provider, Name: modelName}
	}

	s.mu.Lock()
	s.cache = m
	s.mu.Unlock()
	return nil
}

// Get returns the ModelRef for a given role (e.g. "primary", "compact").
// Returns zero ModelRef if not found.
func (s *ModelConfigStore) Get(role string) ModelRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cache[role]
}

// ForRouter returns "provider:model_name" for use with LLMRouter.
// Falls back to the model name alone if provider is empty.
func (s *ModelConfigStore) ForRouter(role string) string {
	ref := s.Get(role)
	if ref.Name == "" {
		return ""
	}
	return ref.ForRouter()
}

// Refresh reloads config from DB. Call after /reset or DB update.
func (s *ModelConfigStore) Refresh(ctx context.Context) error {
	return s.Load(ctx)
}
