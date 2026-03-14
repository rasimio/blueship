package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// UserProfile represents a user in the system.
type UserProfile struct {
	ID          string          `db:"id" json:"id"`
	ChatID      string          `db:"chat_id" json:"chat_id"`
	DisplayName string          `db:"display_name" json:"display_name"`
	TrustLevel  string          `db:"trust_level" json:"trust_level"`
	Bio         json.RawMessage `db:"bio" json:"bio"`
	Preferences json.RawMessage `db:"preferences" json:"preferences"`
	IsOwner     bool            `db:"is_owner" json:"is_owner"`
	Timezone    string          `db:"timezone" json:"timezone"`
	CreatedAt   time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time       `db:"updated_at" json:"updated_at"`
}

// UserStore provides access to user profiles stored in the ship database.
type UserStore interface {
	ResolveByChatID(ctx context.Context, chatID string) (uuid.UUID, error)
	ResolveOwner(ctx context.Context) (uuid.UUID, error)
	GetByID(ctx context.Context, id string) (*UserProfile, error)
	GetByChatID(ctx context.Context, chatID string) (*UserProfile, error)
	ListAll(ctx context.Context) ([]UserProfile, error)
	IsOwner(ctx context.Context, id string) (bool, error)
	Create(ctx context.Context, chatID, displayName, trustLevel, timezone string) (*UserProfile, error)
	Update(ctx context.Context, id, displayName, trustLevel, timezone string) (*UserProfile, error)
	Delete(ctx context.Context, id string) error
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

const userProfileColumns = `id, chat_id, display_name, trust_level, bio, preferences, is_owner, timezone, created_at, updated_at`

func (s *userStore) GetByID(ctx context.Context, id string) (*UserProfile, error) {
	var p UserProfile
	err := s.db.GetContext(ctx, &p,
		`SELECT `+userProfileColumns+` FROM user_profiles WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *userStore) GetByChatID(ctx context.Context, chatID string) (*UserProfile, error) {
	var p UserProfile
	err := s.db.GetContext(ctx, &p,
		`SELECT `+userProfileColumns+` FROM user_profiles WHERE chat_id = $1`, chatID)
	if err != nil {
		return nil, fmt.Errorf("get user by chat_id: %w", err)
	}
	return &p, nil
}

func (s *userStore) ListAll(ctx context.Context) ([]UserProfile, error) {
	var users []UserProfile
	err := s.db.SelectContext(ctx, &users,
		`SELECT `+userProfileColumns+` FROM user_profiles ORDER BY created_at ASC`)
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

func (s *userStore) Create(ctx context.Context, chatID, displayName, trustLevel, timezone string) (*UserProfile, error) {
	if chatID == "" || displayName == "" {
		return nil, fmt.Errorf("chat_id and display_name are required")
	}
	if trustLevel == "" {
		trustLevel = "new"
	}
	if timezone == "" {
		timezone = "Europe/Moscow"
	}

	var p UserProfile
	err := s.db.GetContext(ctx, &p, `
		INSERT INTO user_profiles (chat_id, display_name, trust_level, timezone)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (chat_id) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			trust_level = EXCLUDED.trust_level,
			timezone = EXCLUDED.timezone,
			updated_at = NOW()
		RETURNING `+userProfileColumns, chatID, displayName, trustLevel, timezone)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return &p, nil
}

func (s *userStore) Update(ctx context.Context, id, displayName, trustLevel, timezone string) (*UserProfile, error) {
	var p UserProfile
	err := s.db.GetContext(ctx, &p, `
		UPDATE user_profiles SET
			display_name = COALESCE(NULLIF($2, ''), display_name),
			trust_level = COALESCE(NULLIF($3, ''), trust_level),
			timezone = COALESCE(NULLIF($4, ''), timezone),
			updated_at = NOW()
		WHERE id = $1
		RETURNING `+userProfileColumns, id, displayName, trustLevel, timezone)
	if err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}
	return &p, nil
}

func (s *userStore) Delete(ctx context.Context, id string) error {
	// Prevent deleting owner
	var isOwner bool
	if err := s.db.GetContext(ctx, &isOwner,
		`SELECT is_owner FROM user_profiles WHERE id = $1`, id); err != nil {
		return fmt.Errorf("user not found: %s", id)
	}
	if isOwner {
		return fmt.Errorf("cannot delete owner user")
	}

	res, err := s.db.ExecContext(ctx, `DELETE FROM user_profiles WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user not found: %s", id)
	}
	return nil
}
