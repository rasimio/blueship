package migrate

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"
)

//go:embed runtime/*.sql
var runtimeFS embed.FS

// RunRuntime applies embedded runtime migrations to the database.
func RunRuntime(ctx context.Context, db *sqlx.DB, logger *slog.Logger) error {
	return runFromFS(ctx, db, runtimeFS, "runtime", logger)
}

// RunModule applies migrations from a module's embedded FS.
func RunModule(ctx context.Context, db *sqlx.DB, migrations fs.FS, moduleName string, logger *slog.Logger) error {
	return runFromFS(ctx, db, migrations, moduleName, logger)
}

func runFromFS(ctx context.Context, db *sqlx.DB, fsys fs.FS, source string, logger *slog.Logger) error {
	// Ensure migrations table exists
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	// List SQL files
	var files []string
	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".sql") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk migrations: %w", err)
	}

	sort.Strings(files)

	// Check which are already applied
	applied := make(map[string]bool)
	var rows []string
	if err := db.SelectContext(ctx, &rows, `SELECT filename FROM schema_migrations`); err != nil {
		return fmt.Errorf("list applied migrations: %w", err)
	}
	for _, r := range rows {
		applied[r] = true
	}

	// Apply new migrations
	for _, file := range files {
		key := source + "/" + file
		if applied[key] {
			continue
		}

		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}

		logger.Info("applying migration", "source", source, "file", file)

		tx, err := db.BeginTxx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx for %s: %w", file, err)
		}

		if _, err := tx.ExecContext(ctx, string(data)); err != nil {
			tx.Rollback()
			return fmt.Errorf("execute %s: %w", file, err)
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (filename) VALUES ($1)`, key); err != nil {
			tx.Rollback()
			return fmt.Errorf("record %s: %w", file, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s: %w", file, err)
		}

		logger.Info("applied migration", "source", source, "file", file)
	}

	return nil
}
