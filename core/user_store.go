package core

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// UserProfile represents a user in the system.
type UserProfile struct {
	ID          string    `db:"id" json:"id"`
	ChatID      string    `db:"chat_id" json:"chat_id"`
	DisplayName string    `db:"display_name" json:"display_name"`
	TrustLevel  string    `db:"trust_level" json:"trust_level"`
	IsOwner     bool      `db:"is_owner" json:"is_owner"`
	Timezone    string    `db:"timezone" json:"timezone"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

// UserStore provides access to user profiles stored in the ship database.
type UserStore interface {
	ResolveByChatID(ctx context.Context, chatID string) (uuid.UUID, error)
	ResolveOwner(ctx context.Context) (uuid.UUID, error)
	GetByID(ctx context.Context, id string) (*UserProfile, error)
	ListAll(ctx context.Context) ([]UserProfile, error)
	IsOwner(ctx context.Context, id string) (bool, error)
}

type userStore struct {
	db *sqlx.DB
}

// NewUserStore creates a UserStore backed by the user_profiles table.
func NewUserStore(db *sqlx.DB) UserStore {
	return &userStore{db: db}
}

func (s *userStore) ResolveByChatID(ctx context.Context, chatID string) (uuid.UUID, error) {
	var id string
	err := s.db.GetContext(ctx, &id,
		`SELECT id FROM user_profiles WHERE chat_id = $1`, chatID)
	if err != nil {
		if err == sql.ErrNoRows {
			return uuid.Nil, err
		}
		return uuid.Nil, fmt.Errorf("resolve user %s: %w", chatID, err)
	}
	return uuid.Parse(id)
}

func (s *userStore) ResolveOwner(ctx context.Context) (uuid.UUID, error) {
	var id string
	err := s.db.GetContext(ctx, &id,
		`SELECT id FROM user_profiles WHERE is_owner = true LIMIT 1`)
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve owner: %w", err)
	}
	return uuid.Parse(id)
}

func (s *userStore) GetByID(ctx context.Context, id string) (*UserProfile, error) {
	var p UserProfile
	err := s.db.GetContext(ctx, &p,
		`SELECT id, chat_id, display_name, trust_level, is_owner, timezone, created_at, updated_at
		 FROM user_profiles WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *userStore) ListAll(ctx context.Context) ([]UserProfile, error) {
	var users []UserProfile
	err := s.db.SelectContext(ctx, &users,
		`SELECT id, chat_id, display_name, trust_level, is_owner, timezone, created_at, updated_at
		 FROM user_profiles ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

func (s *userStore) IsOwner(ctx context.Context, id string) (bool, error) {
	var isOwner bool
	err := s.db.GetContext(ctx, &isOwner,
		`SELECT is_owner FROM user_profiles WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return isOwner, nil
}
