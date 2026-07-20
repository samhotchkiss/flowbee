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
	"sync"
	"syscall"
	"time"

	"github.com/samhotchkiss/flowbee/internal/content"
	_ "modernc.org/sqlite"
)

type Store struct {
	DB         *sql.DB
	dsn        string // the SQLite path, for a post-close WAL checkpoint (see Close)
	writerLock *os.File
	// writerLockHeld also records the intentionally no-op in-memory fence. Durable
	// process-incarnation writes require callers to pass through AcquireWriterLock
	// even in tests; a nil file alone cannot distinguish that from no acquisition.
	writerLockHeld bool
	// epicWorkerActivationMu spans authoritative material resolution plus the
	// following admission/activation transaction. SQLite serializes transactions,
	// but this closes the smaller in-process window before BEGIN in which a new
	// epic could otherwise escape an activation candidate snapshot.
	epicWorkerActivationMu sync.Mutex

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

	// AllowOwnSourceRepos is the set of repo ids whose own source (internal/, cmd/,
	// tools/, flows/) is NOT the Flowbee control plane's, so the `flowbee_source`
	// content-denylist class is relaxed for them — letting their internal//cmd/ changes
	// self-merge instead of being forced to the human gate. Empty (the default) = every
	// repo is fully protected (the shipped posture). Set from config by the runtime;
	// applied per-job in contentResultTx. NEVER include the repo that IS Flowbee.
	AllowOwnSourceRepos map[string]bool

	// EnableEpicReviewHandoffV2 gates the durable epic review reconciler. It is
	// deliberately off by default until the incident-slice migrations and
	// runtime wiring are enabled together.
	EnableEpicReviewHandoffV2 bool

	// EnableEpicDedicatedWorkersV2 makes the per-epic builder+reviewer lifecycle
	// ledger authoritative. It is a separate activation fence while existing
	// review-pool jobs are migrated; when enabled, native epic review cannot be
	// claimed through the legacy shared-session path.
	EnableEpicDedicatedWorkersV2 bool
	// EpicWorkerCredentialMaterializer mints and fsyncs a one-shot owner-only
	// envelope before the lifecycle action transaction commits. It returns only
	// the hash persisted in SQLite; plaintext is never returned to Store.
	EpicWorkerCredentialMaterializer func(identity, projectID, role, envelopeID string,
		generation int64, expiresAt time.Time) (string, error)
	// EpicWorkerBootstrapMaterialProvider resolves the exact spec and discipline
	// bytes used by immutable dedicated-worker manifests. It is called before the
	// admission/activation transaction; a missing provider or missing byte fails
	// v2 closed.
	EpicWorkerBootstrapMaterialProvider EpicWorkerBootstrapMaterialProvider
	// ProjectActorCredentialMaterializer applies the same one-shot fsync law to
	// Flowbee-created Interactor/Orchestrator credentials.
	ProjectActorCredentialMaterializer func(identity, projectID, role, envelopeID string,
		generation int64, expiresAt time.Time) (string, error)
	// ManagedSessionDriverFreshFor is the maximum age of the exact Driver instance
	// and store-scoped observation cursor accepted by actor API authorization.
	// Zero uses the fail-closed production default of five minutes.
	ManagedSessionDriverFreshFor time.Duration

	// EnableCapacityV2 routes v2 builders/reviewers/operations only through the
	// identity-bound active capacity generation. It never falls back to the legacy
	// freshness-free worker_accounts projection.
	EnableCapacityV2 bool

	// EnableDriverControlOrigin is true only after the Driver has negotiated an
	// authenticated, non-session control-plane sender. The v2.4 capability must
	// be proven against the running Driver, so the zero/default production posture
	// is fail-closed. Tests using a capability fake must opt in explicitly.
	// This gate is checked at every Flowbee-authored message materialization and
	// claim seam; an active database binding alone is never authority.
	EnableDriverControlOrigin bool
	// DriverControlOriginGate, when installed by serve, is the live negotiated
	// Driver capability. It takes precedence over the static test/CLI switch
	// above so a token revocation or daemon downgrade closes every new
	// materialization seam without restarting Flowbee.
	DriverControlOriginGate func() bool
	// DriverControlOriginEndpointGate is the production capability predicate for
	// multi-Driver deployments. Capability is scoped to the exact Driver routing
	// domain that owns the recipient binding; readiness on one endpoint must never
	// authorize a send through another. When installed, endpoint-less/global
	// checks fail closed.
	DriverControlOriginEndpointGate func(hostID, storeID, tmuxServerDomainID string) bool
}

// WriterLock is an OS-enforced exclusive lease for every process that mutates a
// control-plane database outside the serving process. It is intentionally
// independent of an open SQLite connection so destructive file operations such
// as restore can hold the same fence while atomically replacing the database.
type WriterLock struct {
	file *os.File
}

// AcquireWriterLockForDSN takes the same non-blocking writer fence used by
// Store.AcquireWriterLock without opening SQLite. Callers must Close the result.
// Memory databases need no cross-process fence and return a no-op lock.
func AcquireWriterLockForDSN(dsn string) (*WriterLock, error) {
	path, ok := SQLiteFilePath(dsn)
	if !ok {
		return &WriterLock{}, nil
	}
	f, err := os.OpenFile(path+".writer.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open control-plane writer lock: %w", err)
	}
	if err := os.Chmod(path+".writer.lock", 0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secure control-plane writer lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("control-plane writer already active for %s: %w", path, err)
	}
	return &WriterLock{file: f}, nil
}

// Close releases the writer fence. It is safe for a no-op in-memory lock and
// safe to call more than once.
func (l *WriterLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// HasDriverControlOrigin is the single runtime predicate for Flowbee-authored
// terminal messages. The callback is deliberately process-local capability
// state; durable bindings remain inventory and never confer authority.
func (s *Store) HasDriverControlOrigin() bool {
	if s != nil && s.DriverControlOriginEndpointGate != nil {
		return false
	}
	if s != nil && s.DriverControlOriginGate != nil {
		return s.DriverControlOriginGate()
	}
	return s != nil && s.EnableDriverControlOrigin
}

// HasDriverControlOriginForBinding proves control-origin authority for the
// exact endpoint tuple carried by an already-resolved durable binding. The
// legacy process-wide seam remains available only when no endpoint gate has
// been installed, which keeps focused pre-multi-endpoint tests compatible while
// making production multi-endpoint routing fail closed.
func (s *Store) HasDriverControlOriginForBinding(binding DriverSessionBinding) bool {
	if s == nil || binding.HostID == "" || binding.StoreID == "" || binding.TmuxServerDomainID == "" {
		return false
	}
	if s.DriverControlOriginEndpointGate != nil {
		return s.DriverControlOriginEndpointGate(binding.HostID, binding.StoreID, binding.TmuxServerDomainID)
	}
	return s.HasDriverControlOrigin()
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
	if path, ok := SQLiteFilePath(dsn); ok {
		for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
			if err := os.Chmod(candidate, 0o600); err != nil && !os.IsNotExist(err) {
				_ = db.Close()
				return nil, fmt.Errorf("secure sqlite file %q: %w", candidate, err)
			}
		}
	}
	return &Store{DB: db, dsn: dsn}, nil
}

// SQLiteFilePath resolves only filesystem-backed SQLite DSNs. Keeping this in
// one exported seam prevents permissions, writer locking, and destructive
// offline operations from disagreeing about file URIs, query parameters, or
// in-memory stores.
func SQLiteFilePath(dsn string) (string, bool) {
	path := strings.TrimPrefix(dsn, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == "" || path == ":memory:" || strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory") {
		return "", false
	}
	return path, true
}

func (s *Store) Ping(ctx context.Context) error { return s.DB.PingContext(ctx) }

// AcquireWriterLock establishes the process-lifetime control-plane writer
// lease. SQLite serializes transactions, but it does not prevent an old serve
// process and its replacement from alternately draining the same outbox. The
// non-blocking OS lock makes overlapping control-plane writers fail at startup.
func (s *Store) AcquireWriterLock() error {
	if s.writerLockHeld {
		return nil
	}
	lock, err := AcquireWriterLockForDSN(s.dsn)
	if err != nil {
		return err
	}
	s.writerLock = lock.file
	s.writerLockHeld = true
	return nil
}

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
	if s.writerLock != nil {
		_ = syscall.Flock(int(s.writerLock.Fd()), syscall.LOCK_UN)
		_ = s.writerLock.Close()
		s.writerLock = nil
	}
	s.writerLockHeld = false
	return err
}
