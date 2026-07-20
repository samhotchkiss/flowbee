package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func embeddedMigrationNames() ([]string, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// PendingMigrations inspects without mutating. Serve uses it while holding the
// writer lock to prove whether an existing database needs a rollback snapshot
// before any forward-only migration is attempted.
func PendingMigrations(ctx context.Context, db *sql.DB) ([]string, error) {
	names, err := embeddedMigrationNames()
	if err != nil {
		return nil, err
	}
	var hasTable int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master
		WHERE type='table' AND name='schema_migrations')`).Scan(&hasTable); err != nil {
		return nil, fmt.Errorf("inspect schema_migrations: %w", err)
	}
	if hasTable == 0 {
		return names, nil
	}
	pending := make([]string, 0)
	for _, name := range names {
		var applied int
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?)`, name).Scan(&applied); err != nil {
			return nil, fmt.Errorf("check migration %s: %w", name, err)
		}
		if applied == 0 {
			pending = append(pending, name)
		}
	}
	return pending, nil
}

// HasUserSchema distinguishes a brand-new empty SQLite file from an existing
// installation. A fresh file has nothing to roll back; any user table means a
// pending migration must be preceded by a verified snapshot.
func HasUserSchema(ctx context.Context, db *sql.DB) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name<>'schema_migrations'`).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect database schema: %w", err)
	}
	return count > 0, nil
}

// MigrateUp applies the embedded migrations idempotently, each in its own
// transaction, recording applied versions in schema_migrations.
func MigrateUp(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	names, err := embeddedMigrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?)`, name,
		).Scan(&n); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if n == 1 {
			continue
		}

		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
	}
	return nil
}
