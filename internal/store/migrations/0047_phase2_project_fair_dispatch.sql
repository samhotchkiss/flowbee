-- 0047: durable Phase-2 weighted-fair project dispatch.
-- Scheduler credit and last-service are control-plane truth, not process
-- memory. A lease grant and the fair turn which selected it are committed in
-- the same SQLite transaction. Occupancy is projected from the lease ledger.

CREATE TABLE project_scheduler_state (
    pool           TEXT NOT NULL,
    project_id     TEXT NOT NULL,
    deficit        INTEGER NOT NULL DEFAULT 0,
    last_served_at TEXT NOT NULL DEFAULT '',
    state_version  INTEGER NOT NULL DEFAULT 1,
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (pool, project_id),
    FOREIGN KEY (project_id) REFERENCES projects(id)
);

CREATE TABLE project_scheduler_turns (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    lease_id        TEXT NOT NULL UNIQUE,
    pool            TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    job_id          TEXT NOT NULL,
    forced_by_age   INTEGER NOT NULL DEFAULT 0,
    decisions_json  TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (job_id) REFERENCES jobs(id),
    FOREIGN KEY (lease_id) REFERENCES leases(lease_id)
);

CREATE INDEX idx_project_scheduler_turns_pool_seq
    ON project_scheduler_turns(pool, seq DESC);

CREATE TABLE project_scheduler_occupancy (
    project_id    TEXT PRIMARY KEY,
    active_leases INTEGER NOT NULL DEFAULT 0 CHECK (active_leases >= 0),
    updated_at    TEXT NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects(id)
);

INSERT INTO project_scheduler_occupancy(project_id,active_leases,updated_at)
SELECT p.id,
       (SELECT COUNT(*) FROM leases l WHERE l.ended_at IS NULL AND l.project_id=p.id),
       datetime('now')
  FROM projects p;

-- Use the job's durable owner instead of NEW.project_id: legacy lease writers
-- omit project_id and 0045's attribution trigger updates it after insert.
CREATE TRIGGER project_scheduler_occupancy_after_lease_insert
AFTER INSERT ON leases
BEGIN
  INSERT INTO project_scheduler_occupancy(project_id,active_leases,updated_at)
  VALUES (
    COALESCE((SELECT project_id FROM jobs WHERE id=NEW.job_id),'default'),
    (SELECT COUNT(*) FROM leases l
      WHERE l.ended_at IS NULL
        AND COALESCE((SELECT project_id FROM jobs WHERE id=l.job_id),'default')=
            COALESCE((SELECT project_id FROM jobs WHERE id=NEW.job_id),'default')),
    datetime('now'))
  ON CONFLICT(project_id) DO UPDATE SET
    active_leases=excluded.active_leases, updated_at=excluded.updated_at;
END;

CREATE TRIGGER project_scheduler_occupancy_after_lease_end
AFTER UPDATE OF ended_at ON leases
BEGIN
  INSERT INTO project_scheduler_occupancy(project_id,active_leases,updated_at)
  VALUES (
    COALESCE((SELECT project_id FROM jobs WHERE id=NEW.job_id),'default'),
    (SELECT COUNT(*) FROM leases l
      WHERE l.ended_at IS NULL
        AND COALESCE((SELECT project_id FROM jobs WHERE id=l.job_id),'default')=
            COALESCE((SELECT project_id FROM jobs WHERE id=NEW.job_id),'default')),
    datetime('now'))
  ON CONFLICT(project_id) DO UPDATE SET
    active_leases=excluded.active_leases, updated_at=excluded.updated_at;
END;
