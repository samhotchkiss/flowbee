-- 0055: signed, project-bound external watchdog heartbeat lease.
--
-- Heartbeats use the existing control-alert ingress authentication envelope but
-- are operational readiness facts, not human alerts. Exact request bytes remain
-- immutable for replay/conflict audit; the lease is the current folded fact.

CREATE TABLE external_watchdog_heartbeat_submissions (
    idempotency_key TEXT PRIMARY KEY,
    body_sha256     TEXT NOT NULL,
    body            BLOB NOT NULL,
    envelope_id     TEXT NOT NULL UNIQUE,
    project_id      TEXT NOT NULL,
    watchdog_id     TEXT NOT NULL,
    target          TEXT NOT NULL,
    sequence        INTEGER NOT NULL,
    observed_at     TEXT NOT NULL,
    received_at     TEXT NOT NULL,
    CHECK (length(idempotency_key) BETWEEN 1 AND 512),
    CHECK (length(body_sha256)=64 AND body_sha256=lower(body_sha256)),
    CHECK (length(body)>0),
    CHECK (sequence>0),
    UNIQUE(project_id,watchdog_id,sequence),
    FOREIGN KEY (project_id) REFERENCES projects(id)
);
CREATE INDEX idx_external_watchdog_heartbeat_project_received
    ON external_watchdog_heartbeat_submissions(project_id,received_at,idempotency_key);

CREATE TABLE external_watchdog_leases (
    project_id          TEXT PRIMARY KEY,
    watchdog_id         TEXT NOT NULL,
    target              TEXT NOT NULL,
    last_sequence       INTEGER NOT NULL,
    last_observed_at    TEXT NOT NULL,
    last_received_at    TEXT NOT NULL,
    last_idempotency_key TEXT NOT NULL,
    CHECK (last_sequence>0),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (last_idempotency_key) REFERENCES external_watchdog_heartbeat_submissions(idempotency_key)
);

CREATE TRIGGER external_watchdog_heartbeat_submissions_immutable_update
BEFORE UPDATE ON external_watchdog_heartbeat_submissions
BEGIN
    SELECT RAISE(ABORT, 'external watchdog heartbeat submission is immutable');
END;
CREATE TRIGGER external_watchdog_heartbeat_submissions_immutable_delete
BEFORE DELETE ON external_watchdog_heartbeat_submissions
BEGIN
    SELECT RAISE(ABORT, 'external watchdog heartbeat submission is immutable');
END;
