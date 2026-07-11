package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// TestMigrateIdempotent: re-running MigrateUp on an already-migrated DB is a no-op.
func TestMigrateIdempotent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "m.db")
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 3; i++ {
		if err := store.MigrateUp(context.Background(), st.DB); err != nil {
			t.Fatalf("migrate run %d: %v", i, err)
		}
	}
	// the M1 spine tables exist and are queryable.
	for _, tbl := range []string{"jobs", "job_events", "leases", "workers", "result_idempotency"} {
		if _, err := st.DB.ExecContext(context.Background(), "SELECT count(*) FROM "+tbl); err != nil {
			t.Fatalf("table %s missing: %v", tbl, err)
		}
	}
}

func TestMigrateUpgradesExistingModelInvocationLedger(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "legacy.db")
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB.ExecContext(ctx, `
		CREATE TABLE model_invocation (
			id TEXT PRIMARY KEY,
			model_id TEXT NOT NULL,
			status TEXT NOT NULL,
			estimated_cost_usd NUMERIC(12,6),
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		INSERT INTO model_invocation (id, model_id, status, estimated_cost_usd)
		VALUES ('legacy-1', 'claude-sonnet-5', 'success', 0.12);`); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatalf("migrate legacy ledger: %v", err)
	}
	for _, col := range []string{"slot_key", "binding_id", "tenant_id", "provider", "attempt_index", "is_fallback", "latency_ms", "ttft_ms", "error_code", "error_message"} {
		if !hasColumn(t, st.DB, "model_invocation", col) {
			t.Fatalf("model_invocation missing upgraded column %s", col)
		}
	}
	var slot string
	if err := st.DB.QueryRowContext(ctx, `SELECT slot_key FROM model_invocation WHERE id = 'legacy-1'`).Scan(&slot); err != nil {
		t.Fatal(err)
	}
	if slot != "unknown" {
		t.Fatalf("legacy slot_key = %q, want unknown", slot)
	}
	_, err = st.DB.ExecContext(ctx, `
		INSERT INTO model_invocation (id, model_id, status)
		VALUES ('new-without-slot', 'claude-sonnet-5', 'success')`)
	if err == nil || !strings.Contains(err.Error(), "slot_key") {
		t.Fatalf("insert without slot err = %v, want slot validation failure", err)
	}
}

func hasColumn(t *testing.T, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table, column string) bool {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `PRAGMA table_info(`+table+`)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}
