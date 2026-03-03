package user

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// Profile represents a user in the multi-tenant system.
type Profile struct {
	ID          string          `db:"id" json:"id"`
	ChatID      string          `db:"chat_id" json:"chat_id"`
	DisplayName string          `db:"display_name" json:"display_name"`
	TrustLevel  string          `db:"trust_level" json:"trust_level"`
	Bio         json.RawMessage `db:"bio" json:"bio"`
	Preferences json.RawMessage `db:"preferences" json:"preferences"`
	Timezone    string          `db:"timezone" json:"timezone"`
	IsOwner     bool            `db:"is_owner" json:"is_owner"`
	CreatedAt   time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time       `db:"updated_at" json:"updated_at"`
}

// ResolveByChatID finds an existing user by chat_id.
// Returns sql.ErrNoRows if the user does not exist.
func ResolveByChatID(ctx context.Context, db *sqlx.DB, chatID string) (uuid.UUID, error) {
	var id string
	err := db.GetContext(ctx, &id,
		`SELECT id FROM user_profiles WHERE chat_id = $1`, chatID)
	if err != nil {
		if err == sql.ErrNoRows {
			return uuid.Nil, err
		}
		return uuid.Nil, fmt.Errorf("resolve user %s: %w", chatID, err)
	}
	return uuid.Parse(id)
}

// ListTelegramChatIDs returns all telegram chat IDs from user_profiles.
func ListTelegramChatIDs(ctx context.Context, db *sqlx.DB) ([]int64, error) {
	var chatIDs []string
	err := db.SelectContext(ctx, &chatIDs,
		`SELECT chat_id FROM user_profiles WHERE chat_id LIKE 'telegram:%' ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list telegram chat_ids: %w", err)
	}

	var ids []int64
	for _, cid := range chatIDs {
		var n int64
		if _, err := fmt.Sscanf(cid, "telegram:%d", &n); err == nil {
			ids = append(ids, n)
		}
	}
	return ids, nil
}

// ResolveOwner returns the UUID of the owner user (is_owner=true).
func ResolveOwner(ctx context.Context, db *sqlx.DB) (uuid.UUID, error) {
	var id string
	err := db.GetContext(ctx, &id,
		`SELECT id FROM user_profiles WHERE is_owner = true LIMIT 1`)
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve owner: %w", err)
	}
	return uuid.Parse(id)
}
