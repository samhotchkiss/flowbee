package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
)

// runRestore replaces the control-plane DB with a verified backup snapshot, safely:
//
//	flowbee restore <snapshot.db>   -- restore from an explicit snapshot
//	flowbee restore --latest        -- restore the newest snapshot in the backup dir
//
// Safety invariants:
//  1. Snapshot is verified (integrity_check + ledger rows > 0) BEFORE touching the live DB.
//  2. The live DB is safety-snapshotted to --dir BEFORE replacement (makes the restore reversible).
//  3. --force is required (the user must explicitly confirm; stop serve first).
//  4. The replace is atomic (write-to-temp + rename) so a crash mid-restore can't corrupt.
//  5. Stale -wal/-shm sidecars from the old DB are removed after the rename.
func runRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	dir := fs.String("dir", envOr("FLOWBEE_BACKUP_DIR", defaultBackupDir()), "directory containing snapshots (matches `flowbee backup --dir`)")
	latest := fs.Bool("latest", false, "restore the newest flowbee-*.db snapshot in --dir")
	force := fs.Bool("force", false, "required: bypass the confirmation gate (stop `flowbee serve` first)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var snapPath string
	switch {
	case *latest && len(fs.Args()) > 0:
		return fmt.Errorf("--latest and an explicit snapshot path are mutually exclusive")
	case *latest:
		p, err := latestSnapshot(*dir)
		if err != nil {
			return err
		}
		snapPath = p
	case len(fs.Args()) == 1:
		snapPath = fs.Args()[0]
	default:
		return fmt.Errorf("usage: flowbee restore <snapshot.db>  OR  flowbee restore --latest")
	}

	if abs, err := filepath.Abs(snapPath); err == nil {
		snapPath = abs
	}

	// Step 1: verify snapshot BEFORE touching anything — abort on corrupt or empty.
	ctx := context.Background()
	fmt.Printf("verifying snapshot %s ...\n", snapPath)
	if err := verifySnapshot(ctx, snapPath); err != nil {
		return fmt.Errorf("snapshot failed verification — refusing to restore: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	liveDB := cfg.DatabaseURL

	// Step 2: require --force + print the confirmation notice so the user knows the scope.
	if !*force {
		fmt.Printf("\nrestore will REPLACE: %s\n", liveDB)
		fmt.Printf("  STOP `flowbee serve` before restoring — a restore under a live server is unsupported.\n")
		fmt.Printf("  The current DB will be safety-snapshotted to %s before replacement.\n", *dir)
		fmt.Printf("  Re-run with --force to proceed.\n\n")
		return fmt.Errorf("--force required")
	}
	fmt.Fprintf(os.Stderr, "WARNING: ensure `flowbee serve` is stopped before a restore\n")

	// Step 3: safety-backup the live DB (makes the restore itself reversible).
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return fmt.Errorf("create backup dir %q: %w", *dir, err)
	}
	if _, statErr := os.Stat(liveDB); statErr == nil {
		ts := time.Now().Format("20060102-150405.000")
		safetySnap := filepath.Join(*dir, "pre-restore-"+ts+".db")
		if err := copyFile(liveDB, safetySnap); err != nil {
			return fmt.Errorf("safety backup of live DB failed — aborting: %w", err)
		}
		fmt.Printf("  safety backup → %s\n", safetySnap)
	} else {
		fmt.Printf("  (no existing DB at %s — skipping safety backup)\n", liveDB)
	}

	// Step 4: atomic replace — copy snapshot to a sibling temp file, then rename over the
	// live DB. Same directory → rename is atomic on a single filesystem; a crash mid-copy
	// leaves the temp file (not the live DB) incomplete.
	liveDir := filepath.Dir(liveDB)
	tmp, err := os.CreateTemp(liveDir, ".flowbee-restore-*.db")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	if err := copyFile(snapPath, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("stage snapshot to temp: %w", err)
	}
	if err := os.Rename(tmpPath, liveDB); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic rename: %w", err)
	}

	// Step 5: remove stale WAL/SHM sidecars — they belong to the replaced DB and must not
	// be replayed against the snapshot (different DB identity = data corruption).
	for _, suf := range []string{"-wal", "-shm"} {
		if err := os.Remove(liveDB + suf); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: could not remove %s%s: %v\n", liveDB, suf, err)
		}
	}

	fi, _ := os.Stat(liveDB)
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	fmt.Printf("✓ restored: %s → %s (%d bytes)\n", snapPath, liveDB, size)
	fmt.Println("  (jobs table is a pure fold of the append-only ledger — restore is internally consistent)")
	return nil
}

// latestSnapshot returns the path of the most recent flowbee-*.db file in dir
// (lexical sort == chronological for the YYYYMMDD-HHMMSS.mmm timestamp format).
func latestSnapshot(dir string) (string, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "flowbee-*.db"))
	if err != nil || len(entries) == 0 {
		return "", fmt.Errorf("no snapshots found in %q", dir)
	}
	sort.Strings(entries)
	return entries[len(entries)-1], nil
}

// copyFile copies src to dst, creating or truncating dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
