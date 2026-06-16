package store_test

import (
	"context"
	"path/filepath"
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
