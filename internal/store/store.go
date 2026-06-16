// Package store is the only I/O seam the deterministic core touches: a SQLite
// database (pure-Go modernc driver, so the binary stays CGO-free and statically
// cross-compilable) plus hand-written SQL. Only the single control-plane process
// ever opens it; workers go over HTTP and never touch the DB.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB *sql.DB

	// NoEligibleWorkerDelay is how long a job may sit `ready` with no compliant
	// worker before the no_eligible_worker alarm fires (I-6). Zero disables
	// auto-arming on enqueue (tests arm timers explicitly). Set by the runtime.
	NoEligibleWorkerDelay time.Duration
}

// Open opens the SQLite database with WAL + a busy timeout. A single open
// connection serializes all access in-process — dead simple and correct for the
// single-writer control plane (and it makes exactly-once leasing trivial: claim
// UPDATEs cannot interleave). A 1-writer/N-reader split is a later optimization.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",   // concurrent readers; durable; litestream-friendly
		"PRAGMA busy_timeout=5000",  // wait, don't error, on a held write lock
		"PRAGMA foreign_keys=ON",    // enforce FK constraints
		"PRAGMA synchronous=NORMAL", // durable under WAL, fast
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("set %q: %w", pragma, err)
		}
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{DB: db}, nil
}

func (s *Store) Ping(ctx context.Context) error { return s.DB.PingContext(ctx) }

func (s *Store) Close() error { return s.DB.Close() }
