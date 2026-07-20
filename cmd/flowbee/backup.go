package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

	snap, size, pruned, err := takeSnapshot(ctx, st.DB, *dir, *keep)
	if err != nil {
		return err
	}
	fmt.Printf("✓ snapshot: %s (%d bytes, integrity ok)\n", snap, size)
	if pruned > 0 {
		fmt.Printf("  pruned %d old snapshot(s), keeping the most recent %d\n", pruned, *keep)
	}
	fmt.Println("  (this is the on-disk FLOOR — run Litestream to object storage for real durability; see docs/operating.md §6)")
	return nil
}

// takeSnapshot writes one verified, pruned snapshot of db into dir and returns its path,
// byte size, and the number of older snapshots pruned. It is the shared core behind both
// the `flowbee backup` command and the control plane's built-in auto-backup loop (serve),
// so a manual and an automatic backup are byte-for-byte the same operation. Safe against a
// live writer: VACUUM INTO copies a consistent, checkpointed snapshot under WAL, and the
// jobs table is a pure fold of the append-only ledger, so any restore is self-consistent.
func takeSnapshot(ctx context.Context, db *sql.DB, dir string, keep int) (path string, size int64, pruned int, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, 0, fmt.Errorf("create backup dir %q: %w", dir, err)
	}
	// millisecond precision so two runs in the same second don't collide (VACUUM INTO
	// errors if the target exists); lexical order still == chronological for pruning.
	ts := time.Now().Format("20060102-150405.000")
	snap := filepath.Join(dir, "flowbee-"+ts+".db")
	// VACUUM INTO writes a consistent, defragmented copy from a read snapshot — safe
	// against a live writer under WAL. (No -wal/-shm sidecars: the copy is checkpointed.)
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", snap); err != nil {
		return "", 0, 0, fmt.Errorf("snapshot: %w", err)
	}
	// a backup you cannot restore is not a backup: verify the snapshot independently.
	if err := verifySnapshot(ctx, snap); err != nil {
		return "", 0, 0, fmt.Errorf("snapshot wrote but FAILED verification (%s): %w", snap, err)
	}
	fi, _ := os.Stat(snap)
	if keep > 0 {
		pruned = pruneSnapshots(dir, keep)
	}
	return snap, fi.Size(), pruned, nil
}

// takePreMigrationSnapshot is the mandatory rollback point for an existing
// database before forward-only migrations. Unlike an operator backup it accepts
// a legitimately empty ledger: schema integrity and the exact pre-migration
// version set are the proof that matters here.
func takePreMigrationSnapshot(ctx context.Context, db *sql.DB, dir string, pending []string) (string, error) {
	if len(pending) == 0 {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create backup dir %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("secure backup dir %q: %w", dir, err)
	}
	stamp := time.Now().Format("20060102-150405.000")
	first, last := strings.TrimSuffix(pending[0], ".sql"), strings.TrimSuffix(pending[len(pending)-1], ".sql")
	path := filepath.Join(dir, fmt.Sprintf("pre-migration-%s-to-%s-%s.db", first, last, stamp))
	if err := takeVerifiedRollbackSnapshot(ctx, db, path); err != nil {
		return "", fmt.Errorf("pre-migration snapshot: %w", err)
	}
	return path, nil
}

// takeVerifiedRollbackSnapshot creates a checkpointed SQLite copy and proves it
// carries the exact applied-migration set, passes quick_check, and has no foreign
// key violations. It accepts an empty ledger, which is valid for a pre-migration
// or pre-restore rollback point.
func takeVerifiedRollbackSnapshot(ctx context.Context, db *sql.DB, path string) error {
	appliedBefore, err := appliedMigrationVersions(ctx, db)
	if err != nil {
		return fmt.Errorf("read source migration versions: %w", err)
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure snapshot: %w", err)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer check.Close()
	var quick string
	if err := check.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quick); err != nil || quick != "ok" {
		if err != nil {
			return fmt.Errorf("verify snapshot: %w", err)
		}
		return fmt.Errorf("verify snapshot: quick_check=%q", quick)
	}
	rows, err := check.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("verify snapshot foreign keys: %w", err)
	}
	hasForeignKeyFailure := rows.Next()
	if closeErr := rows.Close(); closeErr != nil {
		return closeErr
	}
	if hasForeignKeyFailure {
		return errors.New("verify snapshot: foreign_key_check failed")
	}
	appliedAfter, err := appliedMigrationVersions(ctx, check)
	if err != nil {
		return fmt.Errorf("verify snapshot migration versions: %w", err)
	}
	if strings.Join(appliedBefore, "\x00") != strings.Join(appliedAfter, "\x00") {
		return fmt.Errorf("verify snapshot migration versions: source=%v snapshot=%v", appliedBefore, appliedAfter)
	}
	keep = true
	return nil
}

func appliedMigrationVersions(ctx context.Context, db *sql.DB) ([]string, error) {
	var hasTable int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master
		WHERE type='table' AND name='schema_migrations')`).Scan(&hasTable); err != nil {
		return nil, err
	}
	if hasTable == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var versions []string
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

// migrateWithRollbackSnapshot is the only production entry point for applying
// embedded migrations. Existing databases are snapshotted and independently
// verified before the first forward-only migration is attempted; fresh empty
// databases do not need a rollback point. Keeping this policy at the command
// boundary lets store.MigrateUp remain a small primitive for isolated tests
// while making serve, migrate, seed, and offline human bootstrap fail closed in
// exactly the same way.
func migrateWithRollbackSnapshot(ctx context.Context, db *sql.DB, dir string) (string, error) {
	pending, err := store.PendingMigrations(ctx, db)
	if err != nil {
		return "", err
	}
	existing, err := store.HasUserSchema(ctx, db)
	if err != nil {
		return "", err
	}
	var snapshot string
	if existing && len(pending) > 0 {
		snapshot, err = takePreMigrationSnapshot(ctx, db, dir, pending)
		if err != nil {
			return "", fmt.Errorf("refusing migration without a verified rollback snapshot: %w", err)
		}
	}
	if err := store.MigrateUp(ctx, db); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// verifySnapshot opens the freshly-written snapshot read-only and runs an integrity
// check. A newly migrated control plane legitimately has no job_events yet; its
// rollback snapshot is still valuable before the first admission, so event count
// is never used as a proxy for database integrity here.
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

// newestSnapshotAge returns how long ago the most recent flowbee-*.db snapshot in dir was
// written (by file mtime), and ok=false when the dir has no snapshot yet. The serve
// auto-backup loop uses it to decide whether a startup catch-up is due, so a
// frequently-restarted control plane still gets a floor within `interval`.
func newestSnapshotAge(dir string) (time.Duration, bool) {
	entries, err := filepath.Glob(filepath.Join(dir, "flowbee-*.db"))
	if err != nil || len(entries) == 0 {
		return 0, false
	}
	sort.Strings(entries) // lexical == chronological for the timestamped names
	fi, err := os.Stat(entries[len(entries)-1])
	if err != nil {
		return 0, false
	}
	return time.Since(fi.ModTime()), true
}

// defaultBackupDir is a `backups/` sibling of the standard DB location.
func defaultBackupDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".flowbee", "backups")
	}
	return "backups"
}
