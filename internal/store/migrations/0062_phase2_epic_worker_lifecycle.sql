-- 0062: one durable builder and one durable, distinct-family reviewer session
-- intent per v2 epic.  Driver owns the eventual tmux/process identities; this
-- ledger owns Flowbee's lifecycle keys, epochs, bootstrap bytes, and shutdown
-- obligation.
CREATE TABLE IF NOT EXISTS epic_worker_sessions (
    epic_id                  TEXT NOT NULL,
    project_id               TEXT NOT NULL,
    worker_role              TEXT NOT NULL CHECK (worker_role IN ('builder','reviewer')),
    model_family             TEXT NOT NULL,
    worker_identity          TEXT NOT NULL,
    flowbee_identity         TEXT NOT NULL,
    seat_id                  TEXT DEFAULT NULL,
    lifecycle_key            TEXT NOT NULL,
    display_name             TEXT NOT NULL,
    state                    TEXT NOT NULL DEFAULT 'planned' CHECK (state IN
                              ('planned','ensure_pending','active','stop_pending','stopped','held')),
    target_epoch             INTEGER NOT NULL DEFAULT 1 CHECK (target_epoch > 0),
    binding_id               TEXT DEFAULT NULL,
    bootstrap_format         TEXT NOT NULL DEFAULT 'flowbee.worker-bootstrap/v1',
    bootstrap_payload        TEXT NOT NULL CHECK (json_valid(bootstrap_payload)),
    bootstrap_sha256         TEXT NOT NULL,
    bootstrap_state          TEXT NOT NULL DEFAULT 'committed' CHECK (bootstrap_state IN
                              ('committed','route_pending','delivered','acknowledged','held')),
    ensure_action_id         TEXT NOT NULL DEFAULT '',
    stop_action_id           TEXT NOT NULL DEFAULT '',
    state_due_at             TEXT NOT NULL DEFAULT '',
    stopped_at               TEXT NOT NULL DEFAULT '',
    created_at               TEXT NOT NULL,
    updated_at               TEXT NOT NULL,
    PRIMARY KEY (epic_id, worker_role),
    UNIQUE (project_id, lifecycle_key),
    UNIQUE (project_id, worker_identity),
    UNIQUE (project_id, flowbee_identity),
    FOREIGN KEY (epic_id) REFERENCES epics(id) ON DELETE CASCADE,
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (seat_id) REFERENCES seats(id),
    FOREIGN KEY (binding_id) REFERENCES driver_session_bindings(binding_id)
);

CREATE INDEX IF NOT EXISTS idx_epic_worker_sessions_state
    ON epic_worker_sessions(project_id,state,worker_role,updated_at);

-- An epic's worker identity and immutable bootstrap contract cannot be changed
-- in-place.  Relaunch advances target_epoch; a different assignment is a new
-- explicit workflow operation, not an UPDATE that silently moves authority.
CREATE TRIGGER epic_worker_sessions_immutable_contract
BEFORE UPDATE ON epic_worker_sessions
WHEN NEW.epic_id<>OLD.epic_id OR NEW.project_id<>OLD.project_id
  OR NEW.worker_role<>OLD.worker_role OR NEW.model_family<>OLD.model_family
  OR NEW.worker_identity<>OLD.worker_identity OR NEW.flowbee_identity<>OLD.flowbee_identity
  OR NEW.lifecycle_key<>OLD.lifecycle_key
  OR NEW.display_name<>OLD.display_name OR NEW.bootstrap_format<>OLD.bootstrap_format
  OR NEW.bootstrap_payload<>OLD.bootstrap_payload OR NEW.bootstrap_sha256<>OLD.bootstrap_sha256
BEGIN
    SELECT RAISE(ABORT,'epic worker immutable contract changed');
END;

CREATE TRIGGER epic_worker_sessions_seat_fence
BEFORE UPDATE OF seat_id ON epic_worker_sessions
WHEN OLD.seat_id IS NOT NULL AND OLD.seat_id<>'' AND NEW.seat_id<>OLD.seat_id
BEGIN
    SELECT RAISE(ABORT,'epic worker seat changed after reservation');
END;

-- Secret bytes live only in the credential installer's owner-only one-shot
-- envelope. This table stores its non-secret effect identity and lifecycle so
-- expiry/rotation/revocation never rewrites the immutable bootstrap contract.
CREATE TABLE IF NOT EXISTS epic_worker_credentials (
    epic_id             TEXT NOT NULL,
    project_id          TEXT NOT NULL,
    worker_role         TEXT NOT NULL CHECK (worker_role IN ('builder','reviewer')),
    flowbee_identity    TEXT NOT NULL,
    install_ref         TEXT NOT NULL,
    state               TEXT NOT NULL DEFAULT 'planned' CHECK (state IN
                         ('planned','issued','installed','refresh_pending','expired','revoked')),
    generation          INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
    envelope_ref        TEXT NOT NULL DEFAULT '',
    payload_sha256      TEXT NOT NULL DEFAULT '',
    refresh_lineage     TEXT NOT NULL DEFAULT '',
    ensure_action_id    TEXT NOT NULL DEFAULT '',
    issued_at           TEXT NOT NULL DEFAULT '',
    refresh_after       TEXT NOT NULL DEFAULT '',
    expires_at          TEXT NOT NULL DEFAULT '',
    installed_at        TEXT NOT NULL DEFAULT '',
    envelope_deleted_at TEXT NOT NULL DEFAULT '',
    revoked_at          TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    PRIMARY KEY (epic_id,worker_role),
    UNIQUE (project_id,install_ref),
    FOREIGN KEY (epic_id,worker_role) REFERENCES epic_worker_sessions(epic_id,worker_role)
        ON DELETE CASCADE,
    FOREIGN KEY (project_id) REFERENCES projects(id)
);

CREATE INDEX IF NOT EXISTS idx_epic_worker_credentials_refresh
    ON epic_worker_credentials(state,refresh_after,expires_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_epic_worker_credentials_envelope
    ON epic_worker_credentials(project_id,envelope_ref) WHERE envelope_ref<>'';

CREATE TRIGGER epic_worker_credentials_immutable_identity
BEFORE UPDATE ON epic_worker_credentials
WHEN NEW.epic_id<>OLD.epic_id OR NEW.project_id<>OLD.project_id
  OR NEW.worker_role<>OLD.worker_role OR NEW.flowbee_identity<>OLD.flowbee_identity
  OR NEW.install_ref<>OLD.install_ref
BEGIN
    SELECT RAISE(ABORT,'epic worker credential identity changed');
END;
