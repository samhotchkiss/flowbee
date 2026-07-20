package bootstrap

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// SQLiteCheckpointStore is the production durable bootstrap ledger. It uses an
// exact version CAS so concurrent CLI/process resumes cannot advance the same
// bootstrap state from stale memory.
type SQLiteCheckpointStore struct{ DB *sql.DB }

const bootstrapCheckpointSchema = `
CREATE TABLE IF NOT EXISTS bootstrap_checkpoints (
    bootstrap_id TEXT PRIMARY KEY,
    plan_sha256 TEXT NOT NULL,
    project_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version >= 1),
    cwd TEXT NOT NULL DEFAULT '',
    repository_origin TEXT NOT NULL DEFAULT '',
    prepared_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(prepared_json) AND json_type(prepared_json) = 'object'),
    issued_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(issued_json) AND json_type(issued_json) = 'object'),
    completed_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(completed_json) AND json_type(completed_json) = 'object'),
    last_hold TEXT NOT NULL DEFAULT '',
    done INTEGER NOT NULL DEFAULT 0 CHECK (done IN (0, 1)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE TRIGGER IF NOT EXISTS bootstrap_checkpoint_identity_immutable
BEFORE UPDATE OF bootstrap_id, plan_sha256, project_id ON bootstrap_checkpoints
BEGIN
    SELECT RAISE(ABORT, 'bootstrap checkpoint identity is immutable');
END;
CREATE TRIGGER IF NOT EXISTS bootstrap_checkpoint_progress_monotonic
BEFORE UPDATE ON bootstrap_checkpoints
BEGIN
    SELECT CASE WHEN OLD.done = 1 AND NEW.done <> 1
        THEN RAISE(ABORT, 'completed bootstrap cannot reopen') END;
    SELECT CASE WHEN OLD.cwd <> '' AND (NEW.cwd <> OLD.cwd OR NEW.repository_origin <> OLD.repository_origin)
        THEN RAISE(ABORT, 'bootstrap project resolution is immutable') END;
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM json_each(OLD.prepared_json) AS old_intent
        LEFT JOIN json_each(NEW.prepared_json) AS new_intent ON new_intent.key = old_intent.key
        WHERE new_intent.key IS NULL OR new_intent.value <> old_intent.value
    ) THEN RAISE(ABORT, 'prepared bootstrap intents are append-only') END;
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM json_each(OLD.issued_json) AS old_effect
        LEFT JOIN json_each(NEW.issued_json) AS new_effect ON new_effect.key = old_effect.key
        WHERE new_effect.key IS NULL OR new_effect.value <> old_effect.value
    ) THEN RAISE(ABORT, 'issued bootstrap effects are append-only') END;
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM json_each(OLD.completed_json) AS old_fact
        LEFT JOIN json_each(NEW.completed_json) AS new_fact ON new_fact.key = old_fact.key
        WHERE new_fact.key IS NULL OR new_fact.value <> old_fact.value
    ) THEN RAISE(ABORT, 'completed bootstrap facts are append-only') END;
END;`

// OpenSQLiteCheckpointStore opens the private machine-local bootstrap ledger.
// It is deliberately separate from flowbee.db, so the pre-serve CLI can never
// become a second writer beside the live control plane.
func OpenSQLiteCheckpointStore(ctx context.Context, path string) (*sql.DB, SQLiteCheckpointStore, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, SQLiteCheckpointStore{}, errors.New("bootstrap ledger path must be exact and absolute")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, SQLiteCheckpointStore{}, fmt.Errorf("create bootstrap ledger directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, SQLiteCheckpointStore{}, fmt.Errorf("secure bootstrap ledger directory: %w", err)
	}
	missing := false
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, SQLiteCheckpointStore{}, errors.New("bootstrap ledger must be a private regular file")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		missing = true
	} else {
		return nil, SQLiteCheckpointStore{}, fmt.Errorf("inspect bootstrap ledger: %w", err)
	}
	if missing {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return nil, SQLiteCheckpointStore{}, fmt.Errorf("create bootstrap ledger: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, SQLiteCheckpointStore{}, fmt.Errorf("close new bootstrap ledger: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, SQLiteCheckpointStore{}, fmt.Errorf("open bootstrap ledger: %w", err)
	}
	closeOnError := func(cause error) (*sql.DB, SQLiteCheckpointStore, error) {
		_ = db.Close()
		return nil, SQLiteCheckpointStore{}, cause
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return closeOnError(fmt.Errorf("secure bootstrap ledger: %w", err))
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		return closeOnError(fmt.Errorf("configure bootstrap ledger: %w", err))
	}
	if _, err := db.ExecContext(ctx, bootstrapCheckpointSchema); err != nil {
		return closeOnError(fmt.Errorf("initialize bootstrap ledger: %w", err))
	}
	return db, SQLiteCheckpointStore{DB: db}, nil
}

func (s SQLiteCheckpointStore) Load(ctx context.Context, id string) (Checkpoint, bool, error) {
	if s.DB == nil {
		return Checkpoint{}, false, errors.New("bootstrap checkpoint database is required")
	}
	var cp Checkpoint
	var prepared, issued, completed string
	var done int
	err := s.DB.QueryRowContext(ctx, `SELECT bootstrap_id, plan_sha256, project_id, version,
		cwd, repository_origin, prepared_json, issued_json, completed_json, last_hold, done
		FROM bootstrap_checkpoints WHERE bootstrap_id=?`, id).Scan(&cp.BootstrapID, &cp.PlanSHA256,
		&cp.ProjectID, &cp.Version, &cp.CWD, &cp.RepositoryOrigin, &prepared, &issued, &completed,
		&cp.LastHold, &done)
	if errors.Is(err, sql.ErrNoRows) {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("load bootstrap checkpoint: %w", err)
	}
	if err := decodeCheckpointMaps(&cp, prepared, issued, completed); err != nil {
		return Checkpoint{}, false, err
	}
	cp.Done = done != 0
	return cp, true, nil
}

func (s SQLiteCheckpointStore) Create(ctx context.Context, cp Checkpoint) (Checkpoint, error) {
	if s.DB == nil {
		return Checkpoint{}, errors.New("bootstrap checkpoint database is required")
	}
	prepared, issued, completed, err := encodeCheckpointMaps(cp)
	if err != nil {
		return Checkpoint{}, err
	}
	cp.Version = 1
	result, err := s.DB.ExecContext(ctx, `INSERT INTO bootstrap_checkpoints
		(bootstrap_id, plan_sha256, project_id, version, cwd, repository_origin,
		 prepared_json, issued_json, completed_json, last_hold, done)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(bootstrap_id) DO NOTHING`,
		cp.BootstrapID, cp.PlanSHA256, cp.ProjectID,
		cp.Version, cp.CWD, cp.RepositoryOrigin, prepared, issued, completed, cp.LastHold, boolInt(cp.Done))
	if err != nil {
		return Checkpoint{}, fmt.Errorf("create bootstrap checkpoint: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Checkpoint{}, fmt.Errorf("count bootstrap checkpoint create: %w", err)
	}
	if rows != 1 {
		return Checkpoint{}, ErrCheckpointConflict
	}
	return cloneCheckpoint(cp), nil
}

func (s SQLiteCheckpointStore) CompareAndSwap(ctx context.Context, cp Checkpoint,
	expected int64) (Checkpoint, error) {
	if s.DB == nil {
		return Checkpoint{}, errors.New("bootstrap checkpoint database is required")
	}
	prepared, issued, completed, err := encodeCheckpointMaps(cp)
	if err != nil {
		return Checkpoint{}, err
	}
	nextVersion := expected + 1
	result, err := s.DB.ExecContext(ctx, `UPDATE bootstrap_checkpoints SET
		version=?, cwd=?, repository_origin=?, prepared_json=?, issued_json=?, completed_json=?, last_hold=?, done=?,
		updated_at=strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE bootstrap_id=? AND plan_sha256=? AND project_id=? AND version=?`,
		nextVersion, cp.CWD, cp.RepositoryOrigin, prepared, issued, completed, cp.LastHold, boolInt(cp.Done),
		cp.BootstrapID, cp.PlanSHA256, cp.ProjectID, expected)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("update bootstrap checkpoint: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Checkpoint{}, fmt.Errorf("count bootstrap checkpoint update: %w", err)
	}
	if rows != 1 {
		return Checkpoint{}, ErrCheckpointConflict
	}
	cp.Version = nextVersion
	return cloneCheckpoint(cp), nil
}

func encodeCheckpointMaps(cp Checkpoint) (string, string, string, error) {
	if cp.Prepared == nil {
		cp.Prepared = map[string]string{}
	}
	if cp.Issued == nil {
		cp.Issued = map[string]string{}
	}
	if cp.Completed == nil {
		cp.Completed = map[string]string{}
	}
	prepared, err := json.Marshal(cp.Prepared)
	if err != nil {
		return "", "", "", fmt.Errorf("encode prepared bootstrap intents: %w", err)
	}
	issued, err := json.Marshal(cp.Issued)
	if err != nil {
		return "", "", "", fmt.Errorf("encode issued bootstrap effects: %w", err)
	}
	completed, err := json.Marshal(cp.Completed)
	if err != nil {
		return "", "", "", fmt.Errorf("encode completed bootstrap facts: %w", err)
	}
	return string(prepared), string(issued), string(completed), nil
}

func decodeCheckpointMaps(cp *Checkpoint, prepared, issued, completed string) error {
	if err := json.Unmarshal([]byte(prepared), &cp.Prepared); err != nil {
		return fmt.Errorf("decode prepared bootstrap intents: %w", err)
	}
	if err := json.Unmarshal([]byte(issued), &cp.Issued); err != nil {
		return fmt.Errorf("decode issued bootstrap effects: %w", err)
	}
	if err := json.Unmarshal([]byte(completed), &cp.Completed); err != nil {
		return fmt.Errorf("decode completed bootstrap facts: %w", err)
	}
	if cp.Issued == nil {
		cp.Issued = map[string]string{}
	}
	if cp.Prepared == nil {
		cp.Prepared = map[string]string{}
	}
	if cp.Completed == nil {
		cp.Completed = map[string]string{}
	}
	return nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
