-- 0023: Ellie maintenance completed-check ledger.
--
-- The 2026-07-05 contradiction-sweep mitigation deferred pending work by 7 days
-- while MEMORY_EXTRACTION_ENABLED=false. That is an operational brake, not the
-- correctness boundary. This table is the durable content-hash gate that makes
-- contradiction, dedup, reground, and reflection sweeps skip unchanged candidates
-- after a completed check.
CREATE TABLE IF NOT EXISTS ellie_maintenance_checks (
    id                              INTEGER PRIMARY KEY AUTOINCREMENT,
    store_id                        TEXT NOT NULL,
    sweep_type                      TEXT NOT NULL CHECK (sweep_type IN ('contradiction', 'dedup', 'reground', 'reflection')),
    candidate_kind                  TEXT NOT NULL CHECK (candidate_kind IN ('pair', 'cluster', 'memory')),
    candidate_key                   TEXT NOT NULL,
    candidate_members               TEXT NOT NULL,
    candidate_content_hashes        TEXT NOT NULL,
    result_status                   TEXT NOT NULL CHECK (result_status IN ('success', 'no_op', 'refusal', 'non_actionable')),
    checked_at                      TEXT NOT NULL,
    sweep_run_id                    TEXT NOT NULL DEFAULT '',
    created_at                      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at                      TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (store_id, sweep_type, candidate_key)
);

CREATE INDEX IF NOT EXISTS ellie_maintenance_checks_lookup
    ON ellie_maintenance_checks (store_id, sweep_type, candidate_key);
