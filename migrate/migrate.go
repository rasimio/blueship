package migrate

import (
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"
)

//go:embed sql/*.sql
var migrations embed.FS

// Run applies all pending migrations in order.
// Files with .optional.sql suffix: errors are logged as warnings, not fatal.
// File naming convention: NNN_name.sql — NNN prefix determines execution order.
func Run(db *sqlx.DB, logger *slog.Logger) error {
	// 1. Create tracking table
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS blueship_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	// 2. Read and sort migration files
	entries, err := fs.ReadDir(migrations, "sql")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	// 3. Apply each migration
	for _, entry := range entries {
		name := entry.Name()
		optional := strings.HasSuffix(name, ".optional.sql")

		// Check if already applied
		var exists int
		if err := db.Get(&exists, `SELECT 1 FROM blueship_migrations WHERE version = $1`, name); err == nil {
			logger.Info("migration already applied", "version", name)
			continue
		}

		// Read SQL
		data, err := fs.ReadFile(migrations, "sql/"+name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		// Execute in transaction
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for %s: %w", name, err)
		}

		if _, err := tx.Exec(string(data)); err != nil {
			_ = tx.Rollback()
			if optional {
				logger.Warn("optional migration skipped", "version", name, "error", err.Error())
				continue
			}
			return fmt.Errorf("exec migration %s: %w", name, err)
		}

		if _, err := tx.Exec(`INSERT INTO blueship_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}

		logger.Info("migration applied", "version", name)
	}

	return nil
}
