package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// TestCloseCheckpointsWAL: Close folds the WAL into the main db file (TRUNCATE
// checkpoint) so a file-level backup is self-contained and a restart replays no WAL.
// After Close the -wal is empty/absent, and the data is durably in flowbee.db.
func TestCloseCheckpointsWAL(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ckpt.db")

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		Now: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// the WAL is checkpoint-truncated on Close: 0 bytes or absent.
	if fi, err := os.Stat(dbPath + "-wal"); err == nil && fi.Size() > 0 {
		t.Fatalf("WAL not truncated on Close: %d bytes (a file-backup would be incomplete)", fi.Size())
	}
	// the write is durably in the main db file (reopen — no WAL to replay).
	st2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if j, err := st2.GetJob(ctx, "j"); err != nil || j.ID != "j" {
		t.Fatalf("data not durable after checkpoint + reopen: %v", err)
	}
}
