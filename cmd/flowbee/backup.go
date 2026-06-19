package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runBackup takes a consistent local snapshot of the SQLite control-plane DB — the
// documented "coarse floor" for durability (operating.md §6). Litestream streaming to
// object storage is the PRODUCTION answer (continuous, off-disk, survives disk loss);
// this is the on-disk floor that protects against accidental corruption/deletion and
// gives a restore point in one command. Safe to run while `flowbee serve` is live: WAL
// mode lets a separate reader take a consistent snapshot of the latest committed state,
// and because the jobs table is a pure fold of the append-only ledger, any restore is
// internally consistent. Schedule it (cron/launchd) for an ongoing local floor.
func runBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	dir := fs.String("dir", envOr("FLOWBEE_BACKUP_DIR", defaultBackupDir()), "directory to write snapshots into")
	keep := fs.Int("keep", 7, "keep only the most recent N snapshots in --dir (prune older); 0 = keep all")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return fmt.Errorf("create backup dir %q: %w", *dir, err)
	}

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open db %q: %w", cfg.DatabaseURL, err)
	}
	defer st.Close()

	// millisecond precision so two runs in the same second don't collide (VACUUM INTO
	// errors if the target exists); lexical order still == chronological for pruning.
	ts := time.Now().Format("20060102-150405.000")
	snap := filepath.Join(*dir, "flowbee-"+ts+".db")
	// VACUUM INTO writes a consistent, defragmented copy from a read snapshot — safe
	// against a live writer under WAL. (No -wal/-shm sidecars: the copy is checkpointed.)
	if _, err := st.DB.ExecContext(ctx, "VACUUM INTO ?", snap); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}

	// a backup you cannot restore is not a backup: verify the snapshot independently.
	if err := verifySnapshot(ctx, snap); err != nil {
		return fmt.Errorf("snapshot wrote but FAILED verification (%s): %w", snap, err)
	}

	fi, _ := os.Stat(snap)
	fmt.Printf("✓ snapshot: %s (%d bytes, integrity ok)\n", snap, fi.Size())

	if *keep > 0 {
		if pruned := pruneSnapshots(*dir, *keep); pruned > 0 {
			fmt.Printf("  pruned %d old snapshot(s), keeping the most recent %d\n", pruned, *keep)
		}
	}
	fmt.Println("  (this is the on-disk FLOOR — run Litestream to object storage for real durability; see docs/operating.md §6)")
	return nil
}

// verifySnapshot opens the freshly-written snapshot read-only and runs an integrity
// check + a sanity row count, so a corrupt copy is caught at backup time, not restore.
func verifySnapshot(ctx context.Context, path string) error {
	snapStore, err := store.Open(ctx, path)
	if err != nil {
		return fmt.Errorf("reopen snapshot: %w", err)
	}
	defer snapStore.Close()
	var ic string
	if err := snapStore.DB.QueryRowContext(ctx, "PRAGMA integrity_check;").Scan(&ic); err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	if ic != "ok" {
		return fmt.Errorf("integrity_check returned %q", ic)
	}
	// the ledger is the durable record; a snapshot with zero events is suspicious.
	var events int
	_ = snapStore.DB.QueryRowContext(ctx, "SELECT count(*) FROM job_events").Scan(&events)
	if events == 0 {
		return fmt.Errorf("snapshot has 0 ledger events (empty/corrupt source?)")
	}
	return nil
}

// pruneSnapshots keeps the most recent `keep` flowbee-*.db files in dir (lexical sort
// == chronological for the YYYYMMDD-HHMMSS stamp) and deletes the rest. Returns the
// number deleted.
func pruneSnapshots(dir string, keep int) int {
	entries, err := filepath.Glob(filepath.Join(dir, "flowbee-*.db"))
	if err != nil || len(entries) <= keep {
		return 0
	}
	sort.Strings(entries) // oldest first
	var deleted int
	for _, old := range entries[:len(entries)-keep] {
		if os.Remove(old) == nil {
			deleted++
		}
	}
	return deleted
}

// defaultBackupDir is a `backups/` sibling of the standard DB location.
func defaultBackupDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".flowbee", "backups")
	}
	return "backups"
}
