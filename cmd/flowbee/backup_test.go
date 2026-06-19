package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestPruneSnapshots(t *testing.T) {
	dir := t.TempDir()
	for _, ts := range []string{"20260101-000001.000", "20260101-000002.000", "20260101-000003.000"} {
		if err := os.WriteFile(filepath.Join(dir, "flowbee-"+ts+".db"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// a non-matching file must be left untouched.
	if err := os.WriteFile(filepath.Join(dir, "other.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if n := pruneSnapshots(dir, 2); n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	remaining, _ := filepath.Glob(filepath.Join(dir, "flowbee-*.db"))
	if len(remaining) != 2 {
		t.Fatalf("want 2 snapshots remaining, got %d", len(remaining))
	}
	// the OLDEST must be the one deleted (lexical == chronological).
	if _, err := os.Stat(filepath.Join(dir, "flowbee-20260101-000001.000.db")); !os.IsNotExist(err) {
		t.Fatal("oldest snapshot must be pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, "other.db")); err != nil {
		t.Fatal("a non-snapshot file must never be pruned")
	}
	// keep >= count is a no-op.
	if n := pruneSnapshots(dir, 10); n != 0 {
		t.Fatalf("keep>=count must prune nothing, got %d", n)
	}
}

func TestBackupRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flowbee.db")
	ctx := context.Background()

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	// seed a job → a job_created ledger event (verifySnapshot rejects 0-event copies).
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	backupDir := filepath.Join(dir, "backups")
	t.Setenv("FLOWBEE_DATABASE_URL", dbPath)
	t.Setenv("FLOWBEE_BACKUP_DIR", backupDir)
	t.Setenv("FLOWBEE_CONFIG", "") // don't pick up a stray flowbee.yaml

	if err := runBackup([]string{"--keep", "5"}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	snaps, _ := filepath.Glob(filepath.Join(backupDir, "flowbee-*.db"))
	if len(snaps) != 1 {
		t.Fatalf("want exactly 1 snapshot, got %d", len(snaps))
	}
	// the snapshot is independently valid + carries the seeded state.
	if err := verifySnapshot(ctx, snaps[0]); err != nil {
		t.Fatalf("snapshot failed verification: %v", err)
	}
	snapStore, err := store.Open(ctx, snaps[0])
	if err != nil {
		t.Fatal(err)
	}
	defer snapStore.Close()
	if _, err := snapStore.GetJob(ctx, "j"); err != nil {
		t.Fatalf("seeded job not present in snapshot: %v", err)
	}
}
