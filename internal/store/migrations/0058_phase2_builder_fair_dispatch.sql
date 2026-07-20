-- 0058: generic durable scheduler effects for non-job resources.
--
-- 0047 binds fair turns to jobs/leases. Flowbee v2 builder compute is an epic
-- lifecycle action, not a synthetic job; this sibling ledger records the exact
-- resource and immutable effect which consumed one project service turn.
CREATE TABLE project_scheduler_effects (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    pool            TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    resource_kind   TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    effect_kind     TEXT NOT NULL,
    effect_id       TEXT NOT NULL,
    effect_epoch    INTEGER NOT NULL DEFAULT 0,
    forced_by_age   INTEGER NOT NULL DEFAULT 0,
    decisions_json  TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    UNIQUE (pool, effect_kind, effect_id, effect_epoch),
    FOREIGN KEY (project_id) REFERENCES projects(id)
);

CREATE INDEX idx_project_scheduler_effects_pool_seq
    ON project_scheduler_effects(pool, seq DESC);

CREATE INDEX idx_project_scheduler_effects_project_seq
    ON project_scheduler_effects(project_id, seq DESC);
