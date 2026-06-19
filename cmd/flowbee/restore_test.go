package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// seedTestDB creates a db at path, migrates it, seeds one job with the given id, and closes it.
func seedTestDB(t *testing.T, ctx context.Context, dbPath, jobID string) {
	t.Helper()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID:    jobID,
		Kind:  job.KindBuild,
		Flow:  "build",
		Stage: "build",
		Role:  job.RoleEngWorker,
		Now:   time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
}

// TestRestoreRoundTrip: backup a seeded DB → restore into a separate live DB path →
// the seeded job is present and the live_job is gone. Also verifies the pre-restore
// safety backup is written and contains the old live DB state.
func TestRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	backupDir := filepath.Join(dir, "backups")

	// Source DB — the snapshot we will restore from.
	srcDB := filepath.Join(dir, "src.db")
	seedTestDB(t, ctx, srcDB, "src_job")

	t.Setenv("FLOWBEE_DATABASE_URL", srcDB)
	t.Setenv("FLOWBEE_BACKUP_DIR", backupDir)
	t.Setenv("FLOWBEE_CONFIG", "")
	if err := runBackup([]string{"--keep", "10"}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	snaps, _ := filepath.Glob(filepath.Join(backupDir, "flowbee-*.db"))
	if len(snaps) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(snaps))
	}
	snap := snaps[0]

	// Live DB — the restore TARGET has a different job so we can tell them apart.
	liveDB := filepath.Join(dir, "live.db")
	seedTestDB(t, ctx, liveDB, "live_job")
	t.Setenv("FLOWBEE_DATABASE_URL", liveDB)

	if err := runRestore([]string{"--force", snap}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// After restore, liveDB must carry src_job (not live_job).
	restoredStore, err := store.Open(ctx, liveDB)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close()
	if _, err := restoredStore.GetJob(ctx, "src_job"); err != nil {
		t.Fatalf("src_job not present after restore: %v", err)
	}
	if _, err := restoredStore.GetJob(ctx, "live_job"); err == nil {
		t.Fatal("live_job must NOT be present after restore (wrong snapshot was applied)")
	}

	// A pre-restore safety backup must have been written.
	safeties, _ := filepath.Glob(filepath.Join(backupDir, "pre-restore-*.db"))
	if len(safeties) != 1 {
		t.Fatalf("want 1 safety backup, got %d", len(safeties))
	}
	safetyStore, err := store.Open(ctx, safeties[0])
	if err != nil {
		t.Fatal(err)
	}
	defer safetyStore.Close()
	if _, err := safetyStore.GetJob(ctx, "live_job"); err != nil {
		t.Fatalf("safety backup must contain live_job (old DB state): %v", err)
	}
}

// TestRestoreRejectsCorrupt: a corrupt snapshot must be rejected before the live DB is touched.
func TestRestoreRejectsCorrupt(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	corrupt := filepath.Join(dir, "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("not a sqlite database"), 0o644); err != nil {
		t.Fatal(err)
	}

	liveDB := filepath.Join(dir, "live.db")
	seedTestDB(t, ctx, liveDB, "safe_job")

	t.Setenv("FLOWBEE_DATABASE_URL", liveDB)
	t.Setenv("FLOWBEE_BACKUP_DIR", filepath.Join(dir, "backups"))
	t.Setenv("FLOWBEE_CONFIG", "")

	if err := runRestore([]string{"--force", corrupt}); err == nil {
		t.Fatal("restore with corrupt snapshot must fail")
	}

	// Live DB must be untouched.
	liveStore, err := store.Open(ctx, liveDB)
	if err != nil {
		t.Fatal(err)
	}
	defer liveStore.Close()
	if _, err := liveStore.GetJob(ctx, "safe_job"); err != nil {
		t.Fatalf("live DB was altered despite corrupt snapshot: %v", err)
	}
	// No safety backup must have been written (we never got past verification).
	safeties, _ := filepath.Glob(filepath.Join(dir, "backups", "pre-restore-*.db"))
	if len(safeties) != 0 {
		t.Fatalf("safety backup must not exist when verification failed, got %d", len(safeties))
	}
}

// TestRestoreRejectsEmpty: a snapshot with 0 ledger events (migrated but unseeded) is rejected.
func TestRestoreRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	emptyDB := filepath.Join(dir, "empty.db")
	st, err := store.Open(ctx, emptyDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	st.Close()

	liveDB := filepath.Join(dir, "live.db")
	seedTestDB(t, ctx, liveDB, "safe_job")

	t.Setenv("FLOWBEE_DATABASE_URL", liveDB)
	t.Setenv("FLOWBEE_BACKUP_DIR", filepath.Join(dir, "backups"))
	t.Setenv("FLOWBEE_CONFIG", "")

	if err := runRestore([]string{"--force", emptyDB}); err == nil {
		t.Fatal("restore with 0-event snapshot must fail")
	}

	// Live DB must be untouched.
	liveStore, err := store.Open(ctx, liveDB)
	if err != nil {
		t.Fatal(err)
	}
	defer liveStore.Close()
	if _, err := liveStore.GetJob(ctx, "safe_job"); err != nil {
		t.Fatalf("live DB was altered despite empty snapshot: %v", err)
	}
}

// TestRestoreLatest: --latest picks the most recent snapshot in --dir.
func TestRestoreLatest(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	backupDir := filepath.Join(dir, "backups")

	// Create two snapshots in order: older_job, then newer_job.
	for i, jobID := range []string{"older_job", "newer_job"} {
		if i > 0 {
			// Ensure a distinct millisecond timestamp in the backup filename.
			time.Sleep(2 * time.Millisecond)
		}
		srcDB := filepath.Join(dir, fmt.Sprintf("src%d.db", i))
		seedTestDB(t, ctx, srcDB, jobID)
		t.Setenv("FLOWBEE_DATABASE_URL", srcDB)
		t.Setenv("FLOWBEE_BACKUP_DIR", backupDir)
		t.Setenv("FLOWBEE_CONFIG", "")
		if err := runBackup([]string{"--keep", "10"}); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}

	snaps, _ := filepath.Glob(filepath.Join(backupDir, "flowbee-*.db"))
	if len(snaps) != 2 {
		t.Fatalf("want 2 snapshots, got %d", len(snaps))
	}

	liveDB := filepath.Join(dir, "live.db")
	t.Setenv("FLOWBEE_DATABASE_URL", liveDB)

	if err := runRestore([]string{"--force", "--latest", "--dir", backupDir}); err != nil {
		t.Fatalf("restore --latest: %v", err)
	}

	restoredStore, err := store.Open(ctx, liveDB)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close()
	if _, err := restoredStore.GetJob(ctx, "newer_job"); err != nil {
		t.Fatalf("newer_job not present after --latest restore: %v", err)
	}
	if _, err := restoredStore.GetJob(ctx, "older_job"); err == nil {
		t.Fatal("older_job must NOT be present — wrong snapshot selected by --latest")
	}
}
