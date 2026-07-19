-- 0035: durable provider/account probe protection and zero-pool stall clocks.

CREATE TABLE IF NOT EXISTS capacity_probe_backoff (
    scope_kind            TEXT NOT NULL CHECK (scope_kind IN ('provider','account')),
    scope_key             TEXT NOT NULL,
    consecutive_failures  INTEGER NOT NULL DEFAULT 0,
    retry_at              TEXT NOT NULL DEFAULT '',
    last_reason           TEXT NOT NULL DEFAULT '',
    last_failure_at       TEXT NOT NULL DEFAULT '',
    last_success_at       TEXT NOT NULL DEFAULT '',
    updated_at            TEXT NOT NULL,
    PRIMARY KEY (scope_kind, scope_key)
);

CREATE TABLE IF NOT EXISTS capacity_pool_health (
    project_id            TEXT NOT NULL,
    pool                  TEXT NOT NULL,
    provider              TEXT NOT NULL,
    queued_work           INTEGER NOT NULL DEFAULT 0,
    routable_seats        INTEGER NOT NULL DEFAULT 0,
    state                 TEXT NOT NULL CHECK (state IN ('healthy','zero_pending','alerted')),
    first_zero_at         TEXT NOT NULL DEFAULT '',
    last_checked_at       TEXT NOT NULL,
    updated_at            TEXT NOT NULL,
    PRIMARY KEY (project_id, pool, provider),
    FOREIGN KEY (project_id) REFERENCES projects(id)
);
