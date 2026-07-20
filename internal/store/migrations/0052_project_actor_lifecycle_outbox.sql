-- 0052: durable project-actor lifecycle intent and Driver effect outbox.
--
-- Project actors are not epics, so their lifecycle effects cannot safely reuse
-- epic_actions or its receipt foreign key. Every Driver mutation starts from an
-- immutable row here. Raw tmux pane selectors, names, CWDs, PIDs, sockets, and
-- provider prose are deliberately absent; pane_instance_id is Driver's stable
-- incarnation UUID, never a raw %N selector.

CREATE TABLE project_actor_lifecycles (
    project_id                    TEXT NOT NULL,
    role                          TEXT NOT NULL CHECK (role IN ('interactor','orchestrator')),
    actor_id                      TEXT NOT NULL,
    route_state_version           INTEGER NOT NULL CHECK (route_state_version > 0),
    desired_state                 TEXT NOT NULL CHECK (desired_state IN ('active','retired')),
    desired_operation             TEXT NOT NULL CHECK (desired_operation IN ('ensure','stop','reattach','adopt','release')),
    lifecycle_ownership           TEXT NOT NULL CHECK (lifecycle_ownership IN ('external_observed','driver_managed')),
    state                         TEXT NOT NULL CHECK (state IN (
        'awaiting_adopt','adopting','verifying_adopt',
        'awaiting_ensure','ensuring','verifying_ensure',
        'active','awaiting_reattach','reattaching','verifying_reattach',
        'awaiting_stop','stopping','verifying_stop',
        'awaiting_release','releasing','verifying_release',
        'stopped','released','held','failed'
    )),
    state_version                 INTEGER NOT NULL DEFAULT 1 CHECK (state_version > 0),
    action_generation             INTEGER NOT NULL DEFAULT 0 CHECK (action_generation >= 0),
    intent_idempotency_key        TEXT NOT NULL,
    intent_payload_json           TEXT NOT NULL,
    intent_payload_sha256         TEXT NOT NULL,
    instance_ref                  TEXT NOT NULL,
    target_host_id                TEXT NOT NULL,
    target_store_id               TEXT NOT NULL,
    target_server_domain_id       TEXT NOT NULL,
    target_server_id              TEXT NOT NULL,
    lifecycle_key                 TEXT NOT NULL,
    target_epoch                  INTEGER NOT NULL CHECK (target_epoch > 0),
    profile_id                    TEXT NOT NULL,
    workspace_root_id             TEXT NOT NULL DEFAULT '',
    workspace_relative_path       TEXT NOT NULL DEFAULT '',
    external_watch_id             TEXT NOT NULL DEFAULT '',
    expected_binding_id           TEXT NOT NULL DEFAULT '',
    expected_binding_epoch        INTEGER NOT NULL DEFAULT 0 CHECK (expected_binding_epoch >= 0),
    expected_session_id           TEXT NOT NULL DEFAULT '',
    expected_pane_instance_id     TEXT NOT NULL DEFAULT '',
    expected_agent_run_id         TEXT NOT NULL DEFAULT '',
    active_binding_id             TEXT NOT NULL DEFAULT '',
    current_action_id             TEXT NOT NULL DEFAULT '',
    recovery_count                INTEGER NOT NULL DEFAULT 0 CHECK (recovery_count >= 0),
    state_entered_at              TEXT NOT NULL,
    state_due_at                  TEXT NOT NULL DEFAULT '',
    fact_progress_at              TEXT NOT NULL,
    return_state                  TEXT NOT NULL DEFAULT '',
    hold_kind                     TEXT NOT NULL DEFAULT '',
    hold_reason                   TEXT NOT NULL DEFAULT '',
    last_error                    TEXT NOT NULL DEFAULT '',
    alert_pending                 INTEGER NOT NULL DEFAULT 0 CHECK (alert_pending IN (0,1)),
    created_at                    TEXT NOT NULL,
    updated_at                    TEXT NOT NULL,
    PRIMARY KEY (project_id,role,actor_id),
    FOREIGN KEY (project_id) REFERENCES projects(id)
);

CREATE INDEX idx_project_actor_lifecycle_due
    ON project_actor_lifecycles(state,state_due_at);
CREATE INDEX idx_project_actor_lifecycle_route
    ON project_actor_lifecycles(project_id,role,desired_state,state);
CREATE UNIQUE INDEX idx_project_actor_lifecycle_current_action
    ON project_actor_lifecycles(current_action_id) WHERE current_action_id<>'';
CREATE UNIQUE INDEX idx_project_actor_lifecycle_intent_key
    ON project_actor_lifecycles(project_id,intent_idempotency_key);

CREATE TABLE project_actor_lifecycle_actions (
    id                            TEXT PRIMARY KEY,
    project_id                    TEXT NOT NULL,
    role                          TEXT NOT NULL CHECK (role IN ('interactor','orchestrator')),
    actor_id                      TEXT NOT NULL,
    route_state_version           INTEGER NOT NULL CHECK (route_state_version > 0),
    intent_state_version          INTEGER NOT NULL CHECK (intent_state_version > 0),
    action_generation             INTEGER NOT NULL CHECK (action_generation > 0),
    operation                     TEXT NOT NULL CHECK (operation IN ('ensure','stop','reattach','adopt','release')),
    state                         TEXT NOT NULL DEFAULT 'pending' CHECK (state IN (
        'pending','delivering','verifying','acknowledged','dead_letter','cancelled_superseded'
    )),
    action_epoch                  INTEGER NOT NULL DEFAULT 0 CHECK (action_epoch >= 0),
    idempotency_key               TEXT NOT NULL,
    dedup_key                     TEXT NOT NULL UNIQUE,
    payload_json                  TEXT NOT NULL,
    payload_sha256                TEXT NOT NULL,
    instance_ref                  TEXT NOT NULL,
    target_host_id                TEXT NOT NULL,
    target_store_id               TEXT NOT NULL,
    target_server_domain_id       TEXT NOT NULL,
    target_server_id              TEXT NOT NULL,
    lifecycle_ownership           TEXT NOT NULL CHECK (lifecycle_ownership IN ('external_observed','driver_managed')),
    lifecycle_key                 TEXT NOT NULL,
    target_epoch                  INTEGER NOT NULL CHECK (target_epoch > 0),
    profile_id                    TEXT NOT NULL,
    workspace_root_id             TEXT NOT NULL DEFAULT '',
    workspace_relative_path       TEXT NOT NULL DEFAULT '',
    external_watch_id             TEXT NOT NULL DEFAULT '',
    expected_binding_id           TEXT NOT NULL DEFAULT '',
    expected_binding_epoch        INTEGER NOT NULL DEFAULT 0 CHECK (expected_binding_epoch >= 0),
    expected_session_id           TEXT NOT NULL DEFAULT '',
    expected_pane_instance_id     TEXT NOT NULL DEFAULT '',
    expected_agent_run_id         TEXT NOT NULL DEFAULT '',
    lease_id                      TEXT NOT NULL,
    lease_epoch                   INTEGER NOT NULL CHECK (lease_epoch > 0),
    attempts                      INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at               TEXT NOT NULL DEFAULT '',
    claim_owner                   TEXT NOT NULL DEFAULT '',
    claim_deadline_at             TEXT NOT NULL DEFAULT '',
    delivery_started_at           TEXT NOT NULL DEFAULT '',
    acknowledged_at               TEXT NOT NULL DEFAULT '',
    dead_lettered_at              TEXT NOT NULL DEFAULT '',
    recovery_count                INTEGER NOT NULL DEFAULT 0 CHECK (recovery_count >= 0),
    last_error                    TEXT NOT NULL DEFAULT '',
    created_at                    TEXT NOT NULL,
    updated_at                    TEXT NOT NULL,
    UNIQUE (project_id,idempotency_key),
    FOREIGN KEY (project_id,role,actor_id)
        REFERENCES project_actor_lifecycles(project_id,role,actor_id)
);

CREATE UNIQUE INDEX idx_project_actor_action_one_live
    ON project_actor_lifecycle_actions(project_id,role,actor_id)
    WHERE state IN ('pending','delivering','verifying');
CREATE INDEX idx_project_actor_action_due
    ON project_actor_lifecycle_actions(state,next_attempt_at,created_at);
CREATE INDEX idx_project_actor_action_verify
    ON project_actor_lifecycle_actions(state,claim_owner,updated_at);
CREATE INDEX idx_project_actor_action_target
    ON project_actor_lifecycle_actions(
        target_host_id,target_store_id,target_server_domain_id,lifecycle_key,target_epoch
    );

CREATE TABLE project_actor_lifecycle_receipts (
    lifecycle_receipt_id         TEXT PRIMARY KEY,
    action_id                     TEXT NOT NULL UNIQUE,
    action_epoch                  INTEGER NOT NULL CHECK (action_epoch > 0),
    operation                     TEXT NOT NULL CHECK (operation IN ('ensure','stop','reattach','adopt','release')),
    lifecycle_key                 TEXT NOT NULL,
    target_epoch                  INTEGER NOT NULL CHECK (target_epoch > 0),
    lease_id                      TEXT NOT NULL,
    lease_epoch                   INTEGER NOT NULL CHECK (lease_epoch > 0),
    tmux_server_domain_id         TEXT NOT NULL,
    external_watch_id             TEXT NOT NULL DEFAULT '',
    status                        TEXT NOT NULL,
    identity_before_json          TEXT NOT NULL DEFAULT '{}',
    identity_after_json           TEXT NOT NULL DEFAULT '{}',
    absence_observed_at           TEXT NOT NULL DEFAULT '',
    diagnostic_code               TEXT NOT NULL DEFAULT '',
    created_at                    TEXT NOT NULL,
    updated_at                    TEXT NOT NULL,
    FOREIGN KEY (action_id) REFERENCES project_actor_lifecycle_actions(id) ON DELETE CASCADE
);

CREATE TRIGGER trg_project_actor_action_immutable
BEFORE UPDATE OF project_id,role,actor_id,route_state_version,intent_state_version,
    action_generation,operation,idempotency_key,dedup_key,payload_json,payload_sha256,
    instance_ref,target_host_id,target_store_id,target_server_domain_id,target_server_id,
    lifecycle_ownership,lifecycle_key,target_epoch,profile_id,workspace_root_id,
    workspace_relative_path,external_watch_id,expected_binding_id,expected_binding_epoch,
    expected_session_id,expected_pane_instance_id,expected_agent_run_id,lease_id,lease_epoch
ON project_actor_lifecycle_actions
WHEN NEW.project_id<>OLD.project_id OR NEW.role<>OLD.role OR NEW.actor_id<>OLD.actor_id
  OR NEW.route_state_version<>OLD.route_state_version OR NEW.intent_state_version<>OLD.intent_state_version
  OR NEW.action_generation<>OLD.action_generation OR NEW.operation<>OLD.operation
  OR NEW.idempotency_key<>OLD.idempotency_key OR NEW.dedup_key<>OLD.dedup_key
  OR NEW.payload_json<>OLD.payload_json OR NEW.payload_sha256<>OLD.payload_sha256
  OR NEW.instance_ref<>OLD.instance_ref OR NEW.target_host_id<>OLD.target_host_id
  OR NEW.target_store_id<>OLD.target_store_id OR NEW.target_server_domain_id<>OLD.target_server_domain_id
  OR NEW.target_server_id<>OLD.target_server_id OR NEW.lifecycle_ownership<>OLD.lifecycle_ownership
  OR NEW.lifecycle_key<>OLD.lifecycle_key OR NEW.target_epoch<>OLD.target_epoch
  OR NEW.profile_id<>OLD.profile_id OR NEW.workspace_root_id<>OLD.workspace_root_id
  OR NEW.workspace_relative_path<>OLD.workspace_relative_path OR NEW.external_watch_id<>OLD.external_watch_id
  OR NEW.expected_binding_id<>OLD.expected_binding_id OR NEW.expected_binding_epoch<>OLD.expected_binding_epoch
  OR NEW.expected_session_id<>OLD.expected_session_id OR NEW.expected_pane_instance_id<>OLD.expected_pane_instance_id
  OR NEW.expected_agent_run_id<>OLD.expected_agent_run_id OR NEW.lease_id<>OLD.lease_id
  OR NEW.lease_epoch<>OLD.lease_epoch
BEGIN
    SELECT RAISE(ABORT,'project actor lifecycle action identity is immutable');
END;

CREATE TRIGGER trg_project_actor_action_shape
BEFORE INSERT ON project_actor_lifecycle_actions
WHEN
    NEW.target_host_id='' OR NEW.target_store_id='' OR NEW.target_server_domain_id=''
 OR NEW.target_server_id='' OR NEW.lifecycle_key='' OR NEW.profile_id='' OR NEW.lease_id=''
 OR (NEW.operation='adopt' AND NOT (
        NEW.lifecycle_ownership='external_observed' AND NEW.external_watch_id<>''
        AND NEW.workspace_root_id='' AND NEW.workspace_relative_path=''
        AND NEW.expected_binding_id='' AND NEW.expected_binding_epoch=0
        AND NEW.expected_session_id<>'' AND NEW.expected_pane_instance_id<>'' AND NEW.expected_agent_run_id<>''))
 OR (NEW.operation='release' AND NOT (
        NEW.lifecycle_ownership='external_observed' AND NEW.external_watch_id<>''
        AND NEW.workspace_root_id='' AND NEW.workspace_relative_path=''
        AND NEW.expected_binding_id<>'' AND NEW.expected_binding_epoch>0
        AND NEW.expected_session_id<>'' AND NEW.expected_pane_instance_id<>'' AND NEW.expected_agent_run_id<>''))
 OR (NEW.operation='ensure' AND NOT (
        NEW.lifecycle_ownership='driver_managed' AND NEW.external_watch_id=''
        AND NEW.workspace_root_id<>'' AND NEW.workspace_relative_path<>''
        AND NEW.expected_binding_id='' AND NEW.expected_binding_epoch=0
        AND NEW.expected_session_id='' AND NEW.expected_pane_instance_id='' AND NEW.expected_agent_run_id=''))
 OR (NEW.operation='stop' AND NOT (
        NEW.lifecycle_ownership='driver_managed' AND NEW.external_watch_id=''
        AND NEW.workspace_root_id<>'' AND NEW.workspace_relative_path<>''
        AND NEW.expected_binding_id<>'' AND NEW.expected_binding_epoch>0
        AND NEW.expected_session_id<>'' AND NEW.expected_pane_instance_id<>'' AND NEW.expected_agent_run_id<>''))
 OR (NEW.operation='reattach' AND NOT (
        NEW.expected_binding_id<>'' AND NEW.expected_binding_epoch>0
        AND NEW.expected_session_id<>'' AND NEW.expected_pane_instance_id<>'' AND NEW.expected_agent_run_id<>''
        AND ((NEW.lifecycle_ownership='external_observed' AND NEW.external_watch_id<>''
              AND NEW.workspace_root_id='' AND NEW.workspace_relative_path='')
          OR (NEW.lifecycle_ownership='driver_managed' AND NEW.external_watch_id=''
              AND NEW.workspace_root_id<>'' AND NEW.workspace_relative_path<>''))))
BEGIN
    SELECT RAISE(ABORT,'invalid project actor lifecycle action shape');
END;

CREATE TRIGGER trg_project_actor_target_epoch_no_regression
BEFORE UPDATE OF target_epoch ON project_actor_lifecycles
WHEN NEW.target_epoch < OLD.target_epoch
BEGIN
    SELECT RAISE(ABORT,'project actor lifecycle target epoch regressed');
END;

CREATE TRIGGER trg_project_actor_action_dead_letter
AFTER UPDATE OF state ON project_actor_lifecycle_actions
WHEN NEW.state='dead_letter' AND OLD.state<>'dead_letter'
BEGIN
    UPDATE project_actor_lifecycles
       SET state='failed',state_version=state_version+1,state_entered_at=NEW.updated_at,
           state_due_at='',fact_progress_at=NEW.updated_at,hold_kind='action_dead_letter',
           hold_reason=NEW.last_error,last_error=NEW.last_error,alert_pending=1,updated_at=NEW.updated_at
     WHERE project_id=NEW.project_id AND role=NEW.role AND actor_id=NEW.actor_id
       AND current_action_id=NEW.id;
    INSERT OR IGNORE INTO control_alerts
        (id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
    VALUES ('actor-action-dead-' || NEW.id,NEW.project_id,NULL,'project_actor_lifecycle_stalled',
        'project_actor_lifecycle_stalled:' || NEW.id,
        json_object('action_id',NEW.id,'role',NEW.role,'actor_id',NEW.actor_id,
                    'operation',NEW.operation,'last_error',NEW.last_error),
        'pending',NEW.updated_at,NEW.updated_at);
    INSERT INTO control_events
        (project_id,epic_id,kind,actor_kind,actor_id,payload_json,created_at)
    VALUES (NEW.project_id,'','project_actor_lifecycle_action_dead_letter','driver',NEW.actor_id,
        json_object('action_id',NEW.id,'role',NEW.role,'operation',NEW.operation),NEW.updated_at);
END;

CREATE TRIGGER trg_project_actor_receipt_immutable
BEFORE UPDATE OF lifecycle_receipt_id,action_id,action_epoch,operation,lifecycle_key,target_epoch,
    lease_id,lease_epoch,tmux_server_domain_id,external_watch_id,status,
    identity_before_json,identity_after_json,absence_observed_at,diagnostic_code
ON project_actor_lifecycle_receipts
WHEN NEW.lifecycle_receipt_id<>OLD.lifecycle_receipt_id OR NEW.action_id<>OLD.action_id
  OR NEW.action_epoch<>OLD.action_epoch OR NEW.operation<>OLD.operation OR NEW.lifecycle_key<>OLD.lifecycle_key
  OR NEW.target_epoch<>OLD.target_epoch OR NEW.lease_id<>OLD.lease_id
  OR NEW.lease_epoch<>OLD.lease_epoch OR NEW.status<>OLD.status
  OR NEW.tmux_server_domain_id<>OLD.tmux_server_domain_id
  OR NEW.external_watch_id<>OLD.external_watch_id
  OR NEW.identity_before_json<>OLD.identity_before_json
  OR NEW.identity_after_json<>OLD.identity_after_json
  OR NEW.absence_observed_at<>OLD.absence_observed_at
  OR NEW.diagnostic_code<>OLD.diagnostic_code
BEGIN
    SELECT RAISE(ABORT,'project actor lifecycle receipt identity is immutable');
END;
