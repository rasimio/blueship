package core

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// PromptStore provides access to system prompts stored in the ship database.
type PromptStore interface {
	Get(ctx context.Context, key string) (string, error)
	GetAll(ctx context.Context) (map[string]string, error)
}

type promptStore struct {
	db *sqlx.DB
}

// NewPromptStore creates a PromptStore backed by the system_prompts table.
func NewPromptStore(db *sqlx.DB) PromptStore {
	return &promptStore{db: db}
}

func (s *promptStore) Get(ctx context.Context, key string) (string, error) {
	var content string
	err := s.db.GetContext(ctx, &content,
		`SELECT content FROM system_prompts WHERE key = $1 AND content <> ''`, key)
	if err != nil {
		return "", fmt.Errorf("prompt %q: %w", key, err)
	}
	return content, nil
}

func (s *promptStore) GetAll(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryxContext(ctx, `SELECT key, content FROM system_prompts WHERE content <> ''`)
	if err != nil {
		return nil, fmt.Errorf("list prompts: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var key, content string
		if err := rows.Scan(&key, &content); err != nil {
			return nil, err
		}
		result[key] = content
	}
	return result, nil
}
