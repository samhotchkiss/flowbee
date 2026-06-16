// Package testutil provides test-only helpers shared across packages: a real
// SQLite store backed by a fresh temp-file DB per test (no server, no
// containers), migrated and registered for cleanup.
package testutil

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// NewStore opens a fresh, migrated SQLite store on a temp-file DB unique to the
// test, and registers cleanup. The temp file (not :memory:) is used because the
// store pins MaxOpenConns(1); a file DB exercises the real WAL/locking path.
func NewStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "flowbee_test.db")
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.MigrateUp(context.Background(), st.DB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
