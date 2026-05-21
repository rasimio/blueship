// Package toolcatalog publishes the daemon's native tool catalog into the
// vaelum.tool_catalog table so the Vaelum web cabinet can enumerate every
// tool the assistant can run. The daemon is the source of truth for which
// tools exist; the cabinet only reads.
package toolcatalog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/rasimio/blueship/core"
)

// Publish replaces every native row in vaelum.tool_catalog with one row per
// tool in defs. meta supplies each tool's cabinet category and core flag; a
// tool absent from meta defaults to category "general", non-core. The whole
// replace runs in one transaction so a concurrent reader sees a consistent
// set. An empty defs is refused — it would wipe the catalog.
func Publish(ctx context.Context, db *sqlx.DB, defs []core.ToolDefinition, meta map[string]core.ToolMeta, logger *slog.Logger) error {
	if len(defs) == 0 {
		return fmt.Errorf("toolcatalog: refusing to publish an empty tool set")
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("toolcatalog: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM vaelum.tool_catalog WHERE kind = 'native'`); err != nil {
		return fmt.Errorf("toolcatalog: clear native rows: %w", err)
	}
	for _, d := range defs {
		m := meta[d.Name]
		category := m.Category
		if category == "" {
			category = "general"
		}
		var schema any
		if len(d.InputSchema) > 0 {
			schema = string(d.InputSchema)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO vaelum.tool_catalog
			   (name, display_name, description, category, kind, core, provider, input_schema, default_enabled)
			 VALUES ($1, $2, $3, $4, 'native', $5, $6, $7::jsonb, true)`,
			d.Name, displayName(d.Name), d.Description, category, m.Core, m.Provider, schema); err != nil {
			return fmt.Errorf("toolcatalog: insert %s: %w", d.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("toolcatalog: commit: %w", err)
	}
	if logger != nil {
		logger.Info("tool catalog published", "native_tools", len(defs))
	}
	return nil
}

// displayName turns a snake_case tool name into a Title Case label.
func displayName(name string) string {
	words := strings.Split(name, "_")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}
