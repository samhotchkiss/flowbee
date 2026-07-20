-- 0060: durable process-incarnation authority and restart audit.
--
-- The OS writer lock prevents overlapping writers. These tables make the owner
-- that acquired it durable and externally legible across crashes/restarts.
CREATE TABLE control_plane_incarnations (
    incarnation_id             TEXT PRIMARY KEY,
    state                      TEXT NOT NULL CHECK (state IN ('active','stopped','superseded')),
    version                    TEXT NOT NULL,
    source_commit              TEXT NOT NULL DEFAULT '',
    config_posture_sha256      TEXT NOT NULL,
    process_id                 INTEGER NOT NULL,
    started_at                 TEXT NOT NULL,
    stopped_at                 TEXT NOT NULL DEFAULT '',
    stop_reason                TEXT NOT NULL DEFAULT '',
    superseded_by              TEXT NOT NULL DEFAULT '',
    created_at                 TEXT NOT NULL,
    updated_at                 TEXT NOT NULL
);

CREATE UNIQUE INDEX idx_control_plane_one_active
    ON control_plane_incarnations(state) WHERE state='active';

CREATE TABLE control_plane_incarnation_events (
    seq                        INTEGER PRIMARY KEY AUTOINCREMENT,
    incarnation_id             TEXT NOT NULL,
    kind                       TEXT NOT NULL CHECK (kind IN ('started','superseded','stopped')),
    related_incarnation_id     TEXT NOT NULL DEFAULT '',
    reason                     TEXT NOT NULL DEFAULT '',
    created_at                 TEXT NOT NULL,
    FOREIGN KEY (incarnation_id) REFERENCES control_plane_incarnations(incarnation_id)
);

CREATE INDEX idx_control_plane_incarnation_events_id_seq
    ON control_plane_incarnation_events(incarnation_id,seq);

CREATE TABLE control_plane_state (
    singleton                  INTEGER PRIMARY KEY CHECK (singleton=1),
    current_incarnation_id     TEXT NOT NULL,
    state_version              INTEGER NOT NULL DEFAULT 1,
    updated_at                 TEXT NOT NULL,
    FOREIGN KEY (current_incarnation_id) REFERENCES control_plane_incarnations(incarnation_id)
);

CREATE TRIGGER control_plane_incarnation_identity_immutable
BEFORE UPDATE OF incarnation_id,version,source_commit,config_posture_sha256,process_id,started_at
ON control_plane_incarnations
BEGIN
  SELECT RAISE(ABORT,'control-plane incarnation identity is immutable');
END;

CREATE TRIGGER control_plane_incarnation_transition_valid
BEFORE UPDATE OF state ON control_plane_incarnations
WHEN OLD.state<>'active' OR NEW.state NOT IN ('stopped','superseded')
 OR NEW.stopped_at='' OR NEW.stop_reason=''
 OR (NEW.state='superseded' AND NEW.superseded_by='')
 OR (NEW.state='stopped' AND NEW.superseded_by<>'')
BEGIN
  SELECT RAISE(ABORT,'invalid control-plane incarnation transition');
END;

CREATE TRIGGER control_plane_incarnation_terminal_immutable
BEFORE UPDATE ON control_plane_incarnations
WHEN OLD.state<>'active'
BEGIN
  SELECT RAISE(ABORT,'terminal control-plane incarnation is immutable');
END;

CREATE TRIGGER control_plane_incarnation_events_no_update
BEFORE UPDATE ON control_plane_incarnation_events
BEGIN
  SELECT RAISE(ABORT,'control-plane incarnation events are append-only');
END;

CREATE TRIGGER control_plane_incarnation_events_no_delete
BEFORE DELETE ON control_plane_incarnation_events
BEGIN
  SELECT RAISE(ABORT,'control-plane incarnation events are append-only');
END;
