-- M1: the lease thread. SQLite translation of DESIGN §5.1 (BUILD.md): '?'
-- placeholders elsewhere; TEXT timestamps via datetime('now'); INTEGER PRIMARY
-- KEY AUTOINCREMENT for the global event sequence (replaces GENERATED IDENTITY);
-- SQLite supports partial unique indexes + UPDATE...RETURNING (modernc).

-- ── 0002_job_events: the append-only ledger (the spine) ──
CREATE TABLE job_events (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT, -- global total order
    job_id       TEXT    NOT NULL,                  -- ULID
    job_seq      INTEGER NOT NULL,                  -- per-job ordinal (1,2,3,…)
    kind         TEXT    NOT NULL,                  -- ledger.EventKind
    from_state   TEXT,
    to_state     TEXT,
    lease_epoch  INTEGER,                           -- the fence in force when emitted
    actor        TEXT    NOT NULL,                  -- 'system' | 'reconcile' | worker identity
    payload      TEXT    NOT NULL DEFAULT '{}',     -- kind-specific RESOLVED facts (JSON)
    created_at   TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (job_id, job_seq)                        -- per-job append serialization
);
CREATE INDEX job_events_job_id_idx ON job_events (job_id, job_seq);

-- ── 0003_jobs: the projection + LIVE lease columns + partial unique index ──
CREATE TABLE jobs (
    id                  TEXT PRIMARY KEY,                         -- ULID, Flowbee-minted
    kind                TEXT NOT NULL CHECK (kind IN ('spec','build')),
    flow                TEXT NOT NULL CHECK (flow IN ('spec','build')),
    stage               TEXT NOT NULL,
    state               TEXT NOT NULL,
    role                TEXT NOT NULL,
    -- lineage (Domain A)
    chat_ref            TEXT,
    spec_ref            TEXT,
    parent_job          TEXT,
    issue_number        INTEGER,
    pr_number           INTEGER,
    -- SHA binding (build)
    base_sha            TEXT,
    head_sha            TEXT,
    -- spec binding (spec)
    spec_content_hash   TEXT,
    spec_version        INTEGER,
    -- scheduling
    blocked_by          TEXT    NOT NULL DEFAULT '[]',            -- JSON array of ULIDs
    priority            INTEGER NOT NULL DEFAULT 0,
    enqueued_at         TEXT    NOT NULL DEFAULT (datetime('now')),
    -- LIVE lease columns (the §6.3.1 claim mutates these in one statement)
    lease_id            TEXT,
    lease_epoch         INTEGER NOT NULL DEFAULT 0,               -- monotonic fence
    lease_deadline      TEXT,                                     -- absolute Rung-3 cap (RFC3339)
    lease_hb_due        TEXT,
    bound_identity      TEXT,
    bound_model_family  TEXT,
    bound_lens          TEXT,
    -- anti-affinity sibling pointers (null in M1)
    eng_worker_job      TEXT,
    code_reviewer_job   TEXT,
    -- counters (§6.7)
    attempts            INTEGER NOT NULL DEFAULT 0,
    max_attempts        INTEGER NOT NULL DEFAULT 5,
    bounces             INTEGER NOT NULL DEFAULT 0,
    max_bounces         INTEGER NOT NULL DEFAULT 3,
    stall_revocations   INTEGER NOT NULL DEFAULT 0,
    cost_tokens         INTEGER NOT NULL DEFAULT 0,
    cost_ceiling_tokens INTEGER,
    -- verdict (gate stages; unused in M1)
    verdict             TEXT,
    job_seq             INTEGER NOT NULL DEFAULT 0,               -- fold cursor
    updated_at          TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- I-4 structural backstop: at most one active lease per job (§6.3.1). The active
-- state list MUST equal job.ActiveLeaseStates.
CREATE UNIQUE INDEX one_active_lease_per_job
    ON jobs (id)
 WHERE state IN ('leased','building','code_review','merging',
                 'merge_handoff','spec_authoring','spec_review');

CREATE INDEX jobs_ready_idx ON jobs (state, priority DESC, enqueued_at) WHERE state = 'ready';

-- ── 0004_leases: lease history/audit (append-only) ──
CREATE TABLE leases (
    lease_id      TEXT PRIMARY KEY,
    job_id        TEXT    NOT NULL,
    lease_epoch   INTEGER NOT NULL,
    identity      TEXT    NOT NULL,
    model_family  TEXT,
    granted_at    TEXT    NOT NULL DEFAULT (datetime('now')),
    ttl_s         INTEGER NOT NULL,
    deadline      TEXT    NOT NULL,
    ended_at      TEXT,
    end_reason    TEXT,                                           -- completed|released|expired|revoked|superseded
    UNIQUE (job_id, lease_epoch)
);

-- ── 0005_workers: registry + attested caps ──
CREATE TABLE workers (
    worker_id              TEXT PRIMARY KEY,
    identity               TEXT NOT NULL UNIQUE,
    host                   TEXT NOT NULL,
    claimed_capabilities   TEXT NOT NULL DEFAULT '[]',            -- JSON array
    attested_capabilities  TEXT NOT NULL DEFAULT '[]',            -- M1: attested := claimed
    max_concurrent_leases  INTEGER NOT NULL DEFAULT 1,
    attestation_expires_at TEXT NOT NULL,
    registered_at          TEXT NOT NULL DEFAULT (datetime('now')),
    last_seen_at           TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ── 0006_result_idempotency ──
CREATE TABLE result_idempotency (
    job_id          TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    response        TEXT NOT NULL,                                -- JSON
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (job_id, idempotency_key)
);
