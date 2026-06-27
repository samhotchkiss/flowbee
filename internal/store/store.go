// Package store is the only I/O seam the deterministic core touches: a SQLite
// database (pure-Go modernc driver, so the binary stays CGO-free and statically
// cross-compilable) plus hand-written SQL. Only the single control-plane process
// ever opens it; workers go over HTTP and never touch the DB.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/content"
	_ "modernc.org/sqlite"
)

type Store struct {
	DB  *sql.DB
	dsn string // the SQLite path, for a post-close WAL checkpoint (see Close)

	// NoEligibleWorkerDelay is how long a job may sit `ready` with no compliant
	// worker before the no_eligible_worker alarm fires (I-6). Zero disables
	// auto-arming on enqueue (tests arm timers explicitly). Set by the runtime.
	NoEligibleWorkerDelay time.Duration

	// ContentPolicy is the operator-configured content-integrity posture (F2): the
	// size ceilings + an EXTRA denylist that augment the shipped, always-on protected
	// set the content gate runs over a worker's untrusted diff (§9.2, I-11). The zero
	// value is exactly the shipped defaults. Set by the runtime from config.
	ContentPolicy content.Policy

	// DefaultCostCeilingMicroUSD is the operator-configured per-job cost circuit-
	// breaker (§6.7, I-15): when > 0, a metered job that carries NO per-job ceiling
	// of its own inherits this one for the duration of the cost decision, so the
	// existing cost_escalated path engages. 0 (the zero value) = no default ceiling,
	// the shipped posture (cost metered, never capped on spend). Set by the runtime
	// from config.CostCeilingMicroUSD().
	DefaultCostCeilingMicroUSD int64

	// AccountBudgetTokens / AccountWindow drive the F6 PREEMPTIVE usage ceiling: the
	// per-account token budget and reset-window length the usage fold uses to turn the
	// boxes' incremental token reports into a REAL accumulating usage_pct (so dispatch
	// rolls over off a near-exhausted codex login at the ceiling, ~90%, BEFORE its hard
	// 429). A per-account budget_tokens override (enrolled via --accounts) wins over the
	// default here. Zero AccountBudgetTokens disables the estimate (usage_pct then only
	// moves on a 429 — the old binary behavior); zero AccountWindow disables window
	// resets (the accumulator never zeroes). Set by the runtime from config.
	AccountBudgetTokens int64
	AccountWindow       time.Duration

	// AllowOwnSourceRepos is the set of repo ids whose own source (internal/, cmd/,
	// tools/, flows/) is NOT the Flowbee control plane's, so the `flowbee_source`
	// content-denylist class is relaxed for them — letting their internal//cmd/ changes
	// self-merge instead of being forced to the human gate. Empty (the default) = every
	// repo is fully protected (the shipped posture). Set from config by the runtime;
	// applied per-job in contentResultTx. NEVER include the repo that IS Flowbee.
	AllowOwnSourceRepos map[string]bool
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
	return &Store{DB: db, dsn: dsn}, nil
}

func (s *Store) Ping(ctx context.Context) error { return s.DB.PingContext(ctx) }

// DBSizeBytes returns the on-disk size of the SQLite database (main file + WAL + SHM),
// so the operator can SEE the ledger grow: job_events is append-only (the source of
// truth — projection == Fold(events)), so the DB grows with throughput over months.
// SQLite handles multi-GB fine and it is litestream-backed, but a metric makes the
// growth observable + alertable rather than a silent surprise (the one unbounded table
// — pruning it safely is a future opt-in feature, not a default). O(1) stat, never a
// table scan. 0 when the path can't be resolved (e.g., an in-memory test DB).
func (s *Store) DBSizeBytes() int64 {
	path := strings.TrimPrefix(s.dsn, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == "" || path == ":memory:" || strings.Contains(s.dsn, ":memory:") || strings.Contains(s.dsn, "mode=memory") {
		return 0
	}
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(path + suffix); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// Close folds the WAL into the main db file, then closes, so a file-level copy of just
// flowbee.db is self-contained, the next start replays no WAL, and a graceful SIGTERM
// leaves a clean on-disk state. It checkpoints on a FRESH connection AFTER the pool is
// closed: while the control plane is up, a background goroutine can hold the single
// pooled connection, so an in-pool TRUNCATE times out and the WAL stays full. Once the
// pool is closed nothing holds the WAL, and the fresh connection's TRUNCATE checkpoint
// folds every committed frame into flowbee.db and shrinks the WAL to zero. Best-effort —
// a checkpoint failure never blocks shutdown (the WAL is still durable for a replay).
func (s *Store) Close() error {
	err := s.DB.Close()
	if s.dsn != "" {
		if db2, e := sql.Open("sqlite", s.dsn); e == nil {
			_, _ = db2.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)")
			_ = db2.Close()
		}
	}
	return err
}
