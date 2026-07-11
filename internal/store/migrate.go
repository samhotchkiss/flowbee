package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

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

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

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
		if name == "0023_model_router.sql" {
			if err := ensureModelInvocationRouterColumns(ctx, tx); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("upgrade model_invocation ledger: %w", err)
			}
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

type txExecer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func ensureModelInvocationRouterColumns(ctx context.Context, db txExecer) error {
	exists, err := tableExists(ctx, db, "model_invocation")
	if err != nil || !exists {
		return err
	}
	cols, err := tableColumns(ctx, db, "model_invocation")
	if err != nil {
		return err
	}
	add := func(name, definition string) error {
		if cols[name] {
			return nil
		}
		if _, err := db.ExecContext(ctx, "ALTER TABLE model_invocation ADD COLUMN "+definition); err != nil {
			return err
		}
		cols[name] = true
		return nil
	}
	for _, col := range []struct {
		name string
		def  string
	}{
		{"slot_key", "slot_key TEXT"},
		{"binding_id", "binding_id TEXT"},
		{"tenant_id", "tenant_id TEXT"},
		{"provider", "provider TEXT"},
		{"model_id", "model_id TEXT NOT NULL DEFAULT ''"},
		{"status", "status TEXT NOT NULL DEFAULT 'unknown'"},
		{"attempt_index", "attempt_index INTEGER NOT NULL DEFAULT 0"},
		{"is_fallback", "is_fallback INTEGER NOT NULL DEFAULT 0 CHECK (is_fallback IN (0, 1))"},
		{"prompt_tokens", "prompt_tokens INTEGER"},
		{"completion_tokens", "completion_tokens INTEGER"},
		{"total_tokens", "total_tokens INTEGER"},
		{"estimated_cost_usd", "estimated_cost_usd NUMERIC(12,6)"},
		{"latency_ms", "latency_ms INTEGER"},
		{"ttft_ms", "ttft_ms INTEGER"},
		{"error_code", "error_code TEXT"},
		{"error_message", "error_message TEXT"},
		{"created_at", "created_at TEXT NOT NULL DEFAULT (datetime('now'))"},
	} {
		if err := add(col.name, col.def); err != nil {
			return fmt.Errorf("add %s: %w", col.name, err)
		}
	}
	_, err = db.ExecContext(ctx, `
		UPDATE model_invocation
		   SET slot_key = 'unknown'
		 WHERE slot_key IS NULL OR slot_key = ''`)
	return err
}

func tableExists(ctx context.Context, db txExecer, table string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

func tableColumns(ctx context.Context, db txExecer, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		cols[strings.ToLower(name)] = true
	}
	return cols, rows.Err()
}
