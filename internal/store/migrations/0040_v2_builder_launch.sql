-- 0040: durable, Driver-routed builder launch targets.
--
-- A seat is an authenticated capacity identity. It is deliberately not also a
-- Driver routing identity. This table joins the two explicitly so admission can
-- acquire one physical seat and commit one immutable Ensure action without ever
-- deriving authority from a tmux name, cwd, PID, provider prose, or proximity.
CREATE TABLE IF NOT EXISTS builder_driver_targets (
    project_id              TEXT NOT NULL DEFAULT 'default',
    seat_id                 TEXT NOT NULL,
    instance_ref            TEXT NOT NULL,
    tmux_server_instance_id TEXT NOT NULL,
    profile_id              TEXT NOT NULL,
    workspace_root_id       TEXT NOT NULL,
    workspace_relative_base TEXT NOT NULL,
    enabled                 INTEGER NOT NULL DEFAULT 1,
    created_at              TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at              TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (project_id, seat_id),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (seat_id) REFERENCES seats(id),
    FOREIGN KEY (instance_ref) REFERENCES driver_instances(instance_ref)
);
CREATE INDEX IF NOT EXISTS idx_builder_driver_targets_instance
    ON builder_driver_targets(instance_ref, enabled);
