package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func Migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(5048001)`); err != nil {
		return fmt.Errorf("lock schema migration: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS atg_schema_migrations (version TEXT PRIMARY KEY, installed_at TIMESTAMPTZ NOT NULL)`); err != nil {
		return fmt.Errorf("create schema migration ledger: %w", err)
	}
	entries, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list schema migrations: %w", err)
	}
	sort.Strings(entries)
	for _, name := range entries {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		var installed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM atg_schema_migrations WHERE version = $1)`, version).Scan(&installed); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if installed {
			continue
		}
		script, err := migrationFiles.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO atg_schema_migrations(version, installed_at) VALUES ($1, NOW())`, version); err != nil {
			return fmt.Errorf("record migration %s: %w", version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migrations: %w", err)
	}
	return nil
}
