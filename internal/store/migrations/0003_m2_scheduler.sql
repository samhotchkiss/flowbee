-- M2: scheduler core (DAG / priority / aging) + capability match + the
-- no_eligible_worker alarm. SQLite translation per the project overrides:
-- '?' placeholders elsewhere; TEXT timestamps; a hand-rolled durable-timer
-- table (replacing River) with a single epoch-guarded polling goroutine.

-- required_capabilities: the capability tags a worker MUST attest to win this
-- job's lease (e.g. ["role:eng_worker","model_family:codex"]). JSON array.
-- Empty array => no capability requirement (any worker may win).
ALTER TABLE jobs ADD COLUMN required_capabilities TEXT NOT NULL DEFAULT '[]';

-- alarm bookkeeping: when a ready job first becomes un-leasable for "too long"
-- the scheduler arms a no_eligible_worker timer; when it fires the alarm is
-- recorded here (and surfaced on the board / SSE). One row per (job, kind).
CREATE TABLE job_alarms (
    job_id     TEXT NOT NULL,
    kind       TEXT NOT NULL,                 -- e.g. 'no_eligible_worker'
    fired_at   TEXT NOT NULL DEFAULT (datetime('now')),
    detail     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (job_id, kind)
);

-- timers: the hand-rolled durable-timer table that replaces River's cadence
-- role (project override #2). due_at is an RFC3339 instant; ONE polling
-- goroutine claims due rows, epoch-guarded so a stale timer is a no-op. M2 uses
-- exactly one timer kind: no_eligible_worker, armed when a job enters `ready`.
CREATE TABLE timers (
    id          TEXT PRIMARY KEY,             -- ULID
    job_id      TEXT NOT NULL,
    kind        TEXT NOT NULL,                -- 'no_eligible_worker'
    due_at      TEXT NOT NULL,                -- RFC3339 instant (Flowbee clock)
    expected_epoch INTEGER NOT NULL,          -- the lease_epoch in force when armed (the guard)
    fired       INTEGER NOT NULL DEFAULT 0,   -- 0 = pending, 1 = fired/cancelled
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX timers_due_idx ON timers (fired, due_at);
