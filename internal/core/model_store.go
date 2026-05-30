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
	rows, err := s.db.QueryxContext(ctx, `
		SELECT role, provider, model_name, max_tokens, thinking_budget,
		       COALESCE(temperature, 0), COALESCE(thinking_mode, ''), COALESCE(effort, '')
		FROM model_config`)
	if err != nil {
		return err
	}
	defer rows.Close()

	m := make(map[string]ModelRef)
	for rows.Next() {
		var role, provider, modelName, thinkingMode, effort string
		var maxTokens, thinkingBudget int
		var temperature float64
		if err := rows.Scan(&role, &provider, &modelName, &maxTokens, &thinkingBudget, &temperature, &thinkingMode, &effort); err != nil {
			return err
		}
		m[role] = ModelRef{
			Provider:       provider,
			Name:           modelName,
			MaxTokens:      maxTokens,
			ThinkingBudget: thinkingBudget,
			Temperature:    temperature,
			ThinkingMode:   thinkingMode,
			Effort:         effort,
		}
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

// Update sets the model for a given role in DB and refreshes the cache.
func (s *ModelConfigStore) Update(ctx context.Context, role, provider, modelName string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE model_config SET provider = $1, model_name = $2, updated_at = NOW() WHERE role = $3`,
		provider, modelName, role)
	if err != nil {
		return err
	}
	return s.Load(ctx)
}

// Roles returns all configured role names.
func (s *ModelConfigStore) Roles() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	roles := make([]string, 0, len(s.cache))
	for role := range s.cache {
		roles = append(roles, role)
	}
	return roles
}

// Refresh reloads config from DB. Call after /reset or DB update.
func (s *ModelConfigStore) Refresh(ctx context.Context) error {
	return s.Load(ctx)
}
