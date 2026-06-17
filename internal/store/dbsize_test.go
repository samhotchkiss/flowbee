package store_test

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// TestDBSizeBytes: a file-backed DB reports a positive on-disk size (the operator's
// signal for ledger growth); the size is O(1) to read and never a table scan.
func TestDBSizeBytes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if sz := st.DBSizeBytes(); sz <= 0 {
		t.Fatalf("DBSizeBytes on a migrated file DB = %d, want > 0", sz)
	}
}
