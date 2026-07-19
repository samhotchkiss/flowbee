-- 0034: append-only identity-bound capacity observations and atomic generations.
-- The legacy account_windows writer remains available while the v2 flag is off;
-- v2 routing reads only the active generation below and therefore fails closed
-- when collection stops, a seat drifts identity, or a projection is incomplete.

ALTER TABLE seats ADD COLUMN expected_host_id TEXT NOT NULL DEFAULT '';
ALTER TABLE seats ADD COLUMN expected_account_key TEXT NOT NULL DEFAULT '';
ALTER TABLE seats ADD COLUMN expected_credential_lineage TEXT NOT NULL DEFAULT '';
ALTER TABLE seats ADD COLUMN capacity_reserve_pct REAL NOT NULL DEFAULT 10;
ALTER TABLE seats ADD COLUMN account_max_concurrent INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS capacity_generations (
    generation_id       TEXT PRIMARY KEY,
    state               TEXT NOT NULL,
    expected_seats      INTEGER NOT NULL,
    observed_seats      INTEGER NOT NULL,
    input_sha256        TEXT NOT NULL,
    started_at          TEXT NOT NULL,
    committed_at        TEXT NOT NULL DEFAULT '',
    failure_reason      TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS account_usage_observations (
    observation_id            TEXT PRIMARY KEY,
    generation_id             TEXT NOT NULL,
    seat_id                   TEXT NOT NULL,
    host_id                   TEXT NOT NULL,
    provider                  TEXT NOT NULL,
    account_key               TEXT NOT NULL DEFAULT '',
    expected_account_key      TEXT NOT NULL DEFAULT '',
    credential_lineage        TEXT NOT NULL DEFAULT '',
    expected_lineage          TEXT NOT NULL DEFAULT '',
    collector_id              TEXT NOT NULL,
    source                    TEXT NOT NULL,
    trust_state               TEXT NOT NULL,
    integrity_state           TEXT NOT NULL,
    identity_match            INTEGER NOT NULL DEFAULT 0,
    lineage_match             INTEGER NOT NULL DEFAULT 0,
    billing_period_active     INTEGER NOT NULL DEFAULT 0,
    windows_json              TEXT NOT NULL DEFAULT '[]',
    rate_limited              INTEGER NOT NULL DEFAULT 0,
    fetched_at                TEXT NOT NULL DEFAULT '',
    persisted_at              TEXT NOT NULL,
    retry_at                  TEXT NOT NULL DEFAULT '',
    live_unavailable_reason   TEXT NOT NULL DEFAULT '',
    raw_response_sha256       TEXT NOT NULL DEFAULT '',
    accepted                  INTEGER NOT NULL DEFAULT 0,
    rejection_reason          TEXT NOT NULL DEFAULT '',
    adapter_version           TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (generation_id) REFERENCES capacity_generations(generation_id),
    FOREIGN KEY (seat_id) REFERENCES seats(id),
    UNIQUE (generation_id, seat_id)
);
CREATE INDEX IF NOT EXISTS idx_capacity_observations_account_time
    ON account_usage_observations(provider, account_key, fetched_at);
CREATE INDEX IF NOT EXISTS idx_capacity_observations_seat_time
    ON account_usage_observations(seat_id, fetched_at);

CREATE TABLE IF NOT EXISTS capacity_seat_projection (
    seat_id                   TEXT PRIMARY KEY,
    generation_id            TEXT NOT NULL,
    observation_id           TEXT NOT NULL,
    host_id                  TEXT NOT NULL,
    provider                 TEXT NOT NULL,
    account_key              TEXT NOT NULL DEFAULT '',
    source                   TEXT NOT NULL,
    trust_state              TEXT NOT NULL,
    integrity_state          TEXT NOT NULL,
    identity_match           INTEGER NOT NULL DEFAULT 0,
    lineage_match            INTEGER NOT NULL DEFAULT 0,
    billing_period_active    INTEGER NOT NULL DEFAULT 0,
    windows_json             TEXT NOT NULL DEFAULT '[]',
    rate_limited             INTEGER NOT NULL DEFAULT 0,
    fetched_at               TEXT NOT NULL DEFAULT '',
    persisted_at             TEXT NOT NULL,
    live_unavailable_reason  TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (generation_id) REFERENCES capacity_generations(generation_id),
    FOREIGN KEY (observation_id) REFERENCES account_usage_observations(observation_id),
    FOREIGN KEY (seat_id) REFERENCES seats(id)
);

CREATE TABLE IF NOT EXISTS capacity_account_projection (
    provider                 TEXT NOT NULL,
    account_key              TEXT NOT NULL,
    generation_id            TEXT NOT NULL,
    source_observation_id    TEXT NOT NULL,
    trust_state              TEXT NOT NULL,
    windows_json             TEXT NOT NULL DEFAULT '[]',
    rate_limited             INTEGER NOT NULL DEFAULT 0,
    fetched_at               TEXT NOT NULL,
    persisted_at             TEXT NOT NULL,
    PRIMARY KEY (provider, account_key),
    FOREIGN KEY (generation_id) REFERENCES capacity_generations(generation_id),
    FOREIGN KEY (source_observation_id) REFERENCES account_usage_observations(observation_id)
);

CREATE TABLE IF NOT EXISTS capacity_active_generation (
    singleton                INTEGER PRIMARY KEY CHECK (singleton = 1),
    generation_id            TEXT NOT NULL,
    activated_at             TEXT NOT NULL,
    FOREIGN KEY (generation_id) REFERENCES capacity_generations(generation_id)
);
