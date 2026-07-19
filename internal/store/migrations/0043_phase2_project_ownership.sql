-- 0043: Phase 2 project/repository ownership and logical actor routes.
-- Additive/default-backed so existing single-project installs remain unchanged.

ALTER TABLE projects ADD COLUMN state_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE projects ADD COLUMN priority INTEGER NOT NULL DEFAULT 100;
ALTER TABLE projects ADD COLUMN scheduler_weight INTEGER NOT NULL DEFAULT 1;
ALTER TABLE projects ADD COLUMN concurrency_cap INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN pause_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN archived_at TEXT NOT NULL DEFAULT '';

CREATE TABLE project_repos (
    project_id TEXT NOT NULL,
    repo_id    TEXT NOT NULL,
    state      TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','paused','archived')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (project_id, repo_id),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);

INSERT INTO project_repos (project_id,repo_id,state,created_at,updated_at)
SELECT 'default',id,CASE WHEN active=1 THEN 'active' ELSE 'paused' END,created_at,created_at
FROM repos;

CREATE TABLE project_actor_routes (
    project_id    TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('interactor','orchestrator')),
    actor_id      TEXT NOT NULL,
    state         TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','paused')),
    state_version INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    PRIMARY KEY (project_id, role),
    FOREIGN KEY (project_id) REFERENCES projects(id)
);

-- Every human project mutation is a durable, payload-bound command. The
-- command row and its domain mutation are committed in one transaction, so a
-- lost HTTP response can be retried without either duplicating the mutation or
-- allowing an idempotency key to be rebound to different work. Create commands
-- use the portfolio scope; all other commands use their project id.
CREATE TABLE project_commands (
    scope_id        TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('create','state','repo','actor')),
    payload_sha256  TEXT NOT NULL,
    resource_ref    TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    PRIMARY KEY (scope_id, idempotency_key)
);

CREATE INDEX idx_project_commands_resource
    ON project_commands(resource_ref, created_at);

-- Legacy pipeline/cost/audit state gets an explicit default-project owner. New
-- Phase-2 writers must always provide the project; the default preserves old clients
-- during the compatibility window without leaving unattributed rows.
ALTER TABLE jobs ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE job_events ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE outbox ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE audit_log ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';

CREATE INDEX idx_jobs_project_state ON jobs(project_id,state);
CREATE INDEX idx_job_events_project_seq ON job_events(project_id,seq);
CREATE INDEX idx_outbox_project_status ON outbox(project_id,status);
CREATE INDEX idx_audit_log_project_acted ON audit_log(project_id,acted_at);
