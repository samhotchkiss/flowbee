-- 0064: server-owned, project-scoped bootstrap action ledger.
-- The no-argument CLI submits immutable intents through the authenticated API;
-- only the serve writer claims and applies them.

CREATE TABLE bootstrap_actions (
    id                    TEXT PRIMARY KEY,
    bootstrap_id          TEXT NOT NULL,
    project_id            TEXT NOT NULL,
    kind                  TEXT NOT NULL CHECK (kind IN
        ('project_upsert','repository_attach','actor_route','actor_lifecycle','seat_bind','managed_topology')),
    payload_json          TEXT NOT NULL,
    payload_sha256        TEXT NOT NULL,
    state                 TEXT NOT NULL DEFAULT 'pending' CHECK (state IN
        ('pending','claimed','verifying','succeeded','uncertain','held','dead_letter')),
    action_epoch          INTEGER NOT NULL DEFAULT 0,
    claim_owner           TEXT NOT NULL DEFAULT '',
    claim_epoch           INTEGER NOT NULL DEFAULT 0,
    claim_deadline_at     TEXT NOT NULL DEFAULT '',
    attempts              INTEGER NOT NULL DEFAULT 0,
    recovery_count        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at       TEXT NOT NULL DEFAULT '',
    receipt_id            TEXT NOT NULL DEFAULT '',
    receipt_state         TEXT NOT NULL DEFAULT '',
    last_error            TEXT NOT NULL DEFAULT '',
    alert_pending         INTEGER NOT NULL DEFAULT 0 CHECK (alert_pending IN (0,1)),
    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL
);
CREATE INDEX idx_bootstrap_actions_runnable
    ON bootstrap_actions(state,next_attempt_at,created_at,id);
CREATE INDEX idx_bootstrap_actions_project
    ON bootstrap_actions(project_id,updated_at,id);

-- Pre-project audit ledger: project_upsert must be durably auditable before the
-- projects row exists, while normal Flowbee control_events correctly retain a
-- foreign key to an existing project. Every transition lands here; transitions
-- after project creation are also projected into control_events.
CREATE TABLE bootstrap_action_events (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    action_id       TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    kind            TEXT NOT NULL,
    from_state      TEXT NOT NULL DEFAULT '',
    to_state        TEXT NOT NULL DEFAULT '',
    action_epoch    INTEGER NOT NULL DEFAULT 0,
    payload_json    TEXT NOT NULL DEFAULT '{}',
    created_at      TEXT NOT NULL,
    FOREIGN KEY (action_id) REFERENCES bootstrap_actions(id) ON DELETE CASCADE
);
CREATE INDEX idx_bootstrap_action_events_action
    ON bootstrap_action_events(action_id,seq);

CREATE TRIGGER bootstrap_action_identity_immutable
BEFORE UPDATE OF id,bootstrap_id,project_id,kind,payload_json,payload_sha256 ON bootstrap_actions
WHEN OLD.id<>NEW.id OR OLD.bootstrap_id<>NEW.bootstrap_id OR OLD.project_id<>NEW.project_id
  OR OLD.kind<>NEW.kind OR OLD.payload_json<>NEW.payload_json OR OLD.payload_sha256<>NEW.payload_sha256
BEGIN
  SELECT RAISE(ABORT,'bootstrap action identity and payload are immutable');
END;

CREATE TRIGGER bootstrap_action_project_owned
BEFORE INSERT ON bootstrap_actions
WHEN NEW.kind<>'project_upsert'
 AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.id=NEW.project_id AND p.state<>'archived')
BEGIN
  SELECT RAISE(ABORT,'bootstrap action project is missing or archived');
END;
