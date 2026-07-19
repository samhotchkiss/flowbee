package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
	_ "modernc.org/sqlite"
)

func TestPreMigrationSnapshotAcceptsEmptyLedgerAndIsOwnerOnly(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migrations(version TEXT PRIMARY KEY);
		INSERT INTO schema_migrations(version) VALUES ('0047_before.sql');
		CREATE TABLE job_events(seq INTEGER PRIMARY KEY);`); err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	if err := os.Chmod(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := takePreMigrationSnapshot(ctx, db, backupDir, []string{"0048_driver_control_principal.sql"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("snapshot mode=%04o want 0600", got)
	}
	dirInfo, err := os.Stat(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("backup dir mode=%04o want 0700", got)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var count int
	if err := check.QueryRow(`SELECT COUNT(*) FROM job_events`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("empty ledger count=%d err=%v", count, err)
	}
	var version string
	if err := check.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version); err != nil || version != "0047_before.sql" {
		t.Fatalf("snapshot migration version=%q err=%v", version, err)
	}
}

func TestMigrateWithRollbackSnapshotProtectsExistingDatabase(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "source.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE legacy_marker(value TEXT NOT NULL);
		INSERT INTO legacy_marker(value) VALUES ('before');`); err != nil {
		t.Fatal(err)
	}

	snapshot, err := migrateWithRollbackSnapshot(ctx, db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot == "" {
		t.Fatal("existing database migrated without a rollback snapshot")
	}
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil || applied == 0 {
		t.Fatalf("applied migrations=%d err=%v", applied, err)
	}
	check, err := sql.Open("sqlite", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var value string
	if err := check.QueryRow(`SELECT value FROM legacy_marker`).Scan(&value); err != nil || value != "before" {
		t.Fatalf("snapshot marker=%q err=%v", value, err)
	}
	var hasMigrationTable int
	if err := check.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='schema_migrations')`).Scan(&hasMigrationTable); err != nil {
		t.Fatal(err)
	}
	if hasMigrationTable != 0 {
		t.Fatal("snapshot was taken after migrations began")
	}
}

func TestMigrateWithRollbackSnapshotFailsClosedWhenSnapshotCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE legacy_marker(value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	notDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateWithRollbackSnapshot(ctx, db, notDir); err == nil ||
		!strings.Contains(err.Error(), "refusing migration without a verified rollback snapshot") {
		t.Fatalf("err=%v", err)
	}
	var hasMigrationTable int
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='schema_migrations')`).Scan(&hasMigrationTable); err != nil {
		t.Fatal(err)
	}
	if hasMigrationTable != 0 {
		t.Fatal("migration mutated the database after snapshot failure")
	}
}

func TestProductionCommandsUseProtectedMigrationEntryPoint(t *testing.T) {
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "backup.go" {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "store.MigrateUp(") {
			t.Errorf("%s bypasses migrateWithRollbackSnapshot", path)
		}
	}
}

func TestEveryProductionMigrationCallerTakesWriterLockFirst(t *testing.T) {
	for _, path := range []string{"serve.go", "migrate.go", "seed.go", "human.go"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		source := string(body)
		migration := strings.Index(source, "migrateWithRollbackSnapshot(")
		if migration < 0 {
			t.Errorf("%s does not use the protected migration entry point", path)
			continue
		}
		lock := strings.LastIndex(source[:migration], ".AcquireWriterLock()")
		if lock < 0 {
			t.Errorf("%s must acquire the writer lock before protected migration", path)
		}
	}
}

func TestSeedFailsClosedWhileControlPlaneWriterIsActive(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flowbee.db")
	configPath := filepath.Join(dir, "flowbee.yaml")
	if err := os.WriteFile(configPath, []byte("database_url: "+dbPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLOWBEE_CONFIG", configPath)
	t.Setenv("FLOWBEE_DATABASE_URL", "")
	t.Setenv("FLOWBEE_BACKUP_DIR", filepath.Join(dir, "backups"))

	active, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	if err := active.AcquireWriterLock(); err != nil {
		t.Fatal(err)
	}
	if err := runSeed([]string{"--task", "must not be written"}); err == nil ||
		!strings.Contains(err.Error(), "writer to be stopped") {
		t.Fatalf("seed err=%v", err)
	}
	var hasSchemaMigrations int
	if err := active.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='schema_migrations')`).Scan(&hasSchemaMigrations); err != nil {
		t.Fatal(err)
	}
	if hasSchemaMigrations != 0 {
		t.Fatal("seed migrated the database despite another active writer")
	}
}
