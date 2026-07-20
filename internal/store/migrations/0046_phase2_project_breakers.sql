-- 0046: Phase-2 project/repository circuit breakers.
--
-- Breakers are deliberately scoped below the shared fleet. A repository fault
-- can hold only that repository, while a project fault can hold only that
-- project's work. Provider/account incidents remain in the existing global
-- capacity and incident machinery.

CREATE TABLE project_circuit_breakers (
    project_id             TEXT NOT NULL,
    repo_id                TEXT NOT NULL DEFAULT '',
    state                  TEXT NOT NULL CHECK (state IN ('closed','open','half_open')),
    state_version          INTEGER NOT NULL DEFAULT 1,
    failure_kind           TEXT NOT NULL DEFAULT '',
    reason                 TEXT NOT NULL DEFAULT '',
    failure_count          INTEGER NOT NULL DEFAULT 0,
    opened_at              TEXT NOT NULL DEFAULT '',
    probe_due_at           TEXT NOT NULL DEFAULT '',
    probe_owner            TEXT NOT NULL DEFAULT '',
    probe_epoch            INTEGER NOT NULL DEFAULT 0,
    probe_lease_expires_at TEXT NOT NULL DEFAULT '',
    last_recovery_fact     TEXT NOT NULL DEFAULT '',
    created_at             TEXT NOT NULL,
    updated_at             TEXT NOT NULL,
    PRIMARY KEY (project_id, repo_id),
    FOREIGN KEY (project_id) REFERENCES projects(id)
);

CREATE INDEX idx_project_breakers_due
    ON project_circuit_breakers(state, probe_due_at, project_id, repo_id);

-- Append-only transition audit. Operator overrides and automatic probes use
-- the same ledger, so a manual action can never erase the mechanical history.
CREATE TABLE project_circuit_breaker_events (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id      TEXT NOT NULL,
    repo_id         TEXT NOT NULL DEFAULT '',
    kind            TEXT NOT NULL,
    from_state      TEXT NOT NULL,
    to_state        TEXT NOT NULL,
    state_version   INTEGER NOT NULL,
    probe_epoch     INTEGER NOT NULL DEFAULT 0,
    actor_kind      TEXT NOT NULL,
    actor_id        TEXT NOT NULL,
    reason          TEXT NOT NULL DEFAULT '',
    evidence_ref    TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects(id)
);

CREATE INDEX idx_project_breaker_events_scope
    ON project_circuit_breaker_events(project_id, repo_id, seq);

-- Human controls use their own payload-bound command ledger. The command and
-- breaker transition are committed together, so a lost HTTP response is safe
-- to replay and a key can never be rebound to a changed control request.
CREATE TABLE project_circuit_breaker_commands (
    project_id      TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    action          TEXT NOT NULL CHECK (action IN ('open','probe_now')),
    payload_sha256  TEXT NOT NULL,
    repo_id         TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    PRIMARY KEY (project_id, idempotency_key),
    FOREIGN KEY (project_id) REFERENCES projects(id)
);
