-- 0048: tmux-driver v2.4 authenticated control-principal message origin.
--
-- Flowbee-authored product messages are not authored by an agent session.  They
-- therefore carry the authenticated `flowbee-control` principal and bind only
-- the exact recipient incarnation.  Existing session-origin rows remain valid
-- audit history, but new product actions no longer forge a synthetic sender
-- session binding.

ALTER TABLE epic_actions ADD COLUMN sender_principal_id TEXT NOT NULL DEFAULT '';
ALTER TABLE driver_grants ADD COLUMN sender_principal_id TEXT NOT NULL DEFAULT '';
ALTER TABLE driver_receipts ADD COLUMN sender_principal_id TEXT NOT NULL DEFAULT '';
ALTER TABLE work_intent_actions ADD COLUMN sender_principal_id TEXT NOT NULL DEFAULT '';

-- ALTER TABLE cannot add CHECK constraints in SQLite, so enforce the origin
-- union durably at both insert and update seams. Driver lifecycle/domain actions
-- are not messages and intentionally carry no sender; executor_kind='driver' is
-- the precise Flowbee message boundary.
CREATE TRIGGER epic_actions_valid_message_origin_insert
BEFORE INSERT ON epic_actions
WHEN NEW.executor_kind='driver' AND NOT (
    (NEW.sender_principal_id<>'' AND NEW.sender_session_id='' AND NEW.sender_agent_run_id='') OR
    (NEW.sender_principal_id='' AND NEW.sender_session_id<>'' AND NEW.sender_agent_run_id<>'')
)
BEGIN
    SELECT RAISE(ABORT,'driver epic action requires exactly one sender origin');
END;
CREATE TRIGGER epic_actions_valid_message_origin_update
BEFORE UPDATE OF executor_kind,sender_principal_id,sender_session_id,sender_agent_run_id ON epic_actions
WHEN NEW.executor_kind='driver' AND NOT (
    (NEW.sender_principal_id<>'' AND NEW.sender_session_id='' AND NEW.sender_agent_run_id='') OR
    (NEW.sender_principal_id='' AND NEW.sender_session_id<>'' AND NEW.sender_agent_run_id<>'')
)
BEGIN
    SELECT RAISE(ABORT,'driver epic action requires exactly one sender origin');
END;

CREATE TRIGGER driver_grants_valid_origin_insert
BEFORE INSERT ON driver_grants
WHEN NOT (
    (NEW.sender_principal_id<>'' AND NEW.sender_session_id='' AND NEW.sender_agent_run_id='') OR
    (NEW.sender_principal_id='' AND NEW.sender_session_id<>'' AND NEW.sender_agent_run_id<>'')
)
BEGIN
    SELECT RAISE(ABORT,'driver grant requires exactly one sender origin');
END;
CREATE TRIGGER driver_grants_valid_origin_update
BEFORE UPDATE OF sender_principal_id,sender_session_id,sender_agent_run_id ON driver_grants
WHEN NOT (
    (NEW.sender_principal_id<>'' AND NEW.sender_session_id='' AND NEW.sender_agent_run_id='') OR
    (NEW.sender_principal_id='' AND NEW.sender_session_id<>'' AND NEW.sender_agent_run_id<>'')
)
BEGIN
    SELECT RAISE(ABORT,'driver grant requires exactly one sender origin');
END;

CREATE TRIGGER driver_receipts_valid_origin_insert
BEFORE INSERT ON driver_receipts
WHEN NOT (
    (NEW.sender_principal_id<>'' AND NEW.sender_session_id='') OR
    (NEW.sender_principal_id='' AND NEW.sender_session_id<>'')
)
BEGIN
    SELECT RAISE(ABORT,'driver receipt requires exactly one sender origin');
END;
CREATE TRIGGER driver_receipts_valid_origin_update
BEFORE UPDATE OF sender_principal_id,sender_session_id ON driver_receipts
WHEN NOT (
    (NEW.sender_principal_id<>'' AND NEW.sender_session_id='') OR
    (NEW.sender_principal_id='' AND NEW.sender_session_id<>'')
)
BEGIN
    SELECT RAISE(ABORT,'driver receipt requires exactly one sender origin');
END;

CREATE TRIGGER work_intent_actions_valid_origin_insert
BEFORE INSERT ON work_intent_actions
WHEN NOT (
    (NEW.sender_principal_id<>'' AND NEW.sender_binding_id='') OR
    (NEW.sender_principal_id='' AND NEW.sender_binding_id<>'')
)
BEGIN
    SELECT RAISE(ABORT,'work intent action requires exactly one sender origin');
END;
CREATE TRIGGER work_intent_actions_valid_origin_update
BEFORE UPDATE OF sender_principal_id,sender_binding_id ON work_intent_actions
WHEN NOT (
    (NEW.sender_principal_id<>'' AND NEW.sender_binding_id='') OR
    (NEW.sender_principal_id='' AND NEW.sender_binding_id<>'')
)
BEGIN
    SELECT RAISE(ABORT,'work intent action requires exactly one sender origin');
END;

-- SQLite cannot remove the historical NOT NULL + foreign-key constraint from
-- sender_binding_id in place. Rebuild both action ledgers and their evidence
-- children in the same migration so no evidence is cascaded or orphaned.

DROP TRIGGER decision_response_actions_immutable_route;
DROP INDEX idx_decision_response_actions_live_response;
DROP INDEX idx_decision_response_actions_pending;
DROP INDEX idx_decision_response_actions_ack_due;
ALTER TABLE decision_response_action_evidence RENAME TO decision_response_action_evidence_0048_old;
ALTER TABLE decision_response_actions RENAME TO decision_response_actions_0048_old;

CREATE TABLE decision_response_actions (
    id                                  TEXT PRIMARY KEY,
    project_id                          TEXT NOT NULL,
    request_id                          TEXT NOT NULL,
    response_id                         TEXT NOT NULL,
    kind                                TEXT NOT NULL DEFAULT 'notify_interactor'
                                        CHECK (kind='notify_interactor'),
    state                               TEXT NOT NULL DEFAULT 'pending' CHECK (state IN
                                        ('pending','claimed','delivered','acknowledged',
                                         'uncertain','dead_letter','fenced')),
    action_epoch                        INTEGER NOT NULL DEFAULT 0 CHECK (action_epoch >= 0),
    dedup_key                           TEXT NOT NULL UNIQUE,
    payload_json                        TEXT NOT NULL CHECK (json_valid(payload_json)),
    payload_sha256                      TEXT NOT NULL,
    target_actor_id                     TEXT NOT NULL,
    sender_principal_id                 TEXT NOT NULL DEFAULT '',
    sender_binding_id                   TEXT DEFAULT NULL,
    target_binding_id                   TEXT NOT NULL,
    evidence_baseline_store_seq         INTEGER NOT NULL DEFAULT 0,
    evidence_baseline_uncertainty_epoch INTEGER NOT NULL DEFAULT 0,
    grant_id                            TEXT NOT NULL,
    grant_epoch                         INTEGER NOT NULL DEFAULT 0,
    grant_expires_at                    TEXT NOT NULL DEFAULT '',
    claim_owner                         TEXT NOT NULL DEFAULT '',
    claim_deadline_at                   TEXT NOT NULL DEFAULT '',
    delivery_started_at                 TEXT NOT NULL DEFAULT '',
    acknowledged_at                     TEXT NOT NULL DEFAULT '',
    acknowledgement_due_at              TEXT NOT NULL DEFAULT '',
    receipt_ref                         TEXT NOT NULL DEFAULT '',
    attempts                            INTEGER NOT NULL DEFAULT 0,
    next_attempt_at                     TEXT NOT NULL DEFAULT '',
    last_error                          TEXT NOT NULL DEFAULT '',
    dead_lettered_at                    TEXT NOT NULL DEFAULT '',
    created_at                          TEXT NOT NULL,
    updated_at                          TEXT NOT NULL,
    CHECK ((sender_principal_id<>'' AND sender_binding_id IS NULL)
        OR (sender_principal_id='' AND COALESCE(sender_binding_id,'')<>'')),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (request_id) REFERENCES decision_requests(id),
    FOREIGN KEY (response_id) REFERENCES decision_responses(id),
    FOREIGN KEY (sender_binding_id) REFERENCES driver_session_bindings(binding_id),
    FOREIGN KEY (target_binding_id) REFERENCES driver_session_bindings(binding_id)
);

INSERT INTO decision_response_actions
SELECT id,project_id,request_id,response_id,kind,state,action_epoch,dedup_key,
       payload_json,payload_sha256,target_actor_id,'',sender_binding_id,target_binding_id,
       evidence_baseline_store_seq,evidence_baseline_uncertainty_epoch,grant_id,grant_epoch,
       grant_expires_at,claim_owner,claim_deadline_at,delivery_started_at,acknowledged_at,
       acknowledgement_due_at,receipt_ref,attempts,next_attempt_at,last_error,
       dead_lettered_at,created_at,updated_at
  FROM decision_response_actions_0048_old;

CREATE TABLE decision_response_action_evidence (
    action_id          TEXT NOT NULL,
    action_epoch       INTEGER NOT NULL,
    store_id           TEXT NOT NULL,
    event_id           TEXT NOT NULL,
    store_seq          INTEGER NOT NULL,
    session_id         TEXT NOT NULL,
    pane_instance_id   TEXT NOT NULL,
    agent_run_id       TEXT NOT NULL,
    evidence_kind      TEXT NOT NULL,
    payload_sha256     TEXT NOT NULL,
    state              TEXT NOT NULL DEFAULT 'confirmed',
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL,
    PRIMARY KEY (action_id,action_epoch),
    UNIQUE (action_id,event_id),
    FOREIGN KEY (action_id) REFERENCES decision_response_actions(id) ON DELETE CASCADE,
    FOREIGN KEY (store_id,event_id)
        REFERENCES driver_observation_events(store_id,event_id)
);
INSERT INTO decision_response_action_evidence SELECT * FROM decision_response_action_evidence_0048_old;
DROP TABLE decision_response_action_evidence_0048_old;
DROP TABLE decision_response_actions_0048_old;

CREATE UNIQUE INDEX idx_decision_response_actions_live_response
    ON decision_response_actions(response_id) WHERE state<>'fenced';
CREATE INDEX idx_decision_response_actions_pending
    ON decision_response_actions(state,next_attempt_at,created_at);
CREATE INDEX idx_decision_response_actions_ack_due
    ON decision_response_actions(state,acknowledgement_due_at)
    WHERE acknowledgement_due_at<>'';
CREATE TRIGGER decision_response_actions_immutable_route
BEFORE UPDATE ON decision_response_actions
WHEN NEW.project_id<>OLD.project_id OR NEW.request_id<>OLD.request_id
  OR NEW.response_id<>OLD.response_id OR NEW.kind<>OLD.kind
  OR NEW.dedup_key<>OLD.dedup_key OR NEW.payload_json<>OLD.payload_json
  OR NEW.payload_sha256<>OLD.payload_sha256 OR NEW.target_actor_id<>OLD.target_actor_id
  OR NEW.sender_principal_id<>OLD.sender_principal_id
  OR NEW.sender_binding_id IS NOT OLD.sender_binding_id
  OR NEW.target_binding_id<>OLD.target_binding_id
  OR NEW.evidence_baseline_store_seq<>OLD.evidence_baseline_store_seq
  OR NEW.evidence_baseline_uncertainty_epoch<>OLD.evidence_baseline_uncertainty_epoch
BEGIN
    SELECT RAISE(ABORT,'decision response action immutable identity changed');
END;

DROP TRIGGER conversation_message_actions_immutable_route;
DROP INDEX idx_conversation_message_actions_live_message;
DROP INDEX idx_conversation_message_actions_pending;
DROP INDEX idx_conversation_message_actions_ack_due;
ALTER TABLE conversation_message_action_evidence RENAME TO conversation_message_action_evidence_0048_old;
ALTER TABLE conversation_message_actions RENAME TO conversation_message_actions_0048_old;

CREATE TABLE conversation_message_actions (
    id                                  TEXT PRIMARY KEY,
    project_id                          TEXT NOT NULL,
    thread_id                           TEXT NOT NULL,
    message_id                          TEXT NOT NULL,
    kind                                TEXT NOT NULL DEFAULT 'deliver_to_interactor'
                                        CHECK (kind='deliver_to_interactor'),
    state                               TEXT NOT NULL DEFAULT 'pending' CHECK (state IN
                                        ('pending','claimed','delivered','acknowledged',
                                         'uncertain','dead_letter','fenced')),
    action_epoch                        INTEGER NOT NULL DEFAULT 0 CHECK (action_epoch >= 0),
    dedup_key                           TEXT NOT NULL UNIQUE,
    payload_text                        TEXT NOT NULL,
    payload_sha256                      TEXT NOT NULL,
    target_actor_id                     TEXT NOT NULL,
    sender_principal_id                 TEXT NOT NULL DEFAULT '',
    sender_binding_id                   TEXT DEFAULT NULL,
    target_binding_id                   TEXT NOT NULL,
    evidence_baseline_store_seq         INTEGER NOT NULL DEFAULT 0,
    evidence_baseline_uncertainty_epoch INTEGER NOT NULL DEFAULT 0,
    grant_id                            TEXT NOT NULL,
    grant_epoch                         INTEGER NOT NULL DEFAULT 0,
    grant_expires_at                    TEXT NOT NULL DEFAULT '',
    claim_owner                         TEXT NOT NULL DEFAULT '',
    claim_deadline_at                   TEXT NOT NULL DEFAULT '',
    delivery_started_at                 TEXT NOT NULL DEFAULT '',
    acknowledged_at                     TEXT NOT NULL DEFAULT '',
    acknowledgement_due_at              TEXT NOT NULL DEFAULT '',
    receipt_ref                         TEXT NOT NULL DEFAULT '',
    attempts                            INTEGER NOT NULL DEFAULT 0,
    next_attempt_at                     TEXT NOT NULL DEFAULT '',
    last_error                          TEXT NOT NULL DEFAULT '',
    dead_lettered_at                    TEXT NOT NULL DEFAULT '',
    created_at                          TEXT NOT NULL,
    updated_at                          TEXT NOT NULL,
    CHECK ((sender_principal_id<>'' AND sender_binding_id IS NULL)
        OR (sender_principal_id='' AND COALESCE(sender_binding_id,'')<>'')),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (thread_id) REFERENCES conversation_threads(id) ON DELETE CASCADE,
    FOREIGN KEY (message_id) REFERENCES conversation_messages(id) ON DELETE CASCADE,
    FOREIGN KEY (sender_binding_id) REFERENCES driver_session_bindings(binding_id),
    FOREIGN KEY (target_binding_id) REFERENCES driver_session_bindings(binding_id)
);

INSERT INTO conversation_message_actions
SELECT id,project_id,thread_id,message_id,kind,state,action_epoch,dedup_key,
       payload_text,payload_sha256,target_actor_id,'',sender_binding_id,target_binding_id,
       evidence_baseline_store_seq,evidence_baseline_uncertainty_epoch,grant_id,grant_epoch,
       grant_expires_at,claim_owner,claim_deadline_at,delivery_started_at,acknowledged_at,
       acknowledgement_due_at,receipt_ref,attempts,next_attempt_at,last_error,
       dead_lettered_at,created_at,updated_at
  FROM conversation_message_actions_0048_old;

CREATE TABLE conversation_message_action_evidence (
    action_id          TEXT NOT NULL,
    action_epoch       INTEGER NOT NULL,
    store_id           TEXT NOT NULL,
    event_id           TEXT NOT NULL,
    store_seq          INTEGER NOT NULL,
    session_id         TEXT NOT NULL,
    pane_instance_id   TEXT NOT NULL,
    agent_run_id       TEXT NOT NULL,
    evidence_kind      TEXT NOT NULL,
    payload_sha256     TEXT NOT NULL,
    state              TEXT NOT NULL DEFAULT 'confirmed',
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL,
    PRIMARY KEY (action_id,action_epoch),
    UNIQUE (action_id,event_id),
    FOREIGN KEY (action_id) REFERENCES conversation_message_actions(id) ON DELETE CASCADE,
    FOREIGN KEY (store_id,event_id)
        REFERENCES driver_observation_events(store_id,event_id)
);
INSERT INTO conversation_message_action_evidence SELECT * FROM conversation_message_action_evidence_0048_old;
DROP TABLE conversation_message_action_evidence_0048_old;
DROP TABLE conversation_message_actions_0048_old;

CREATE UNIQUE INDEX idx_conversation_message_actions_live_message
    ON conversation_message_actions(message_id) WHERE state<>'fenced';
CREATE INDEX idx_conversation_message_actions_pending
    ON conversation_message_actions(state,next_attempt_at,created_at);
CREATE INDEX idx_conversation_message_actions_ack_due
    ON conversation_message_actions(state,acknowledgement_due_at)
    WHERE acknowledgement_due_at<>'';
CREATE TRIGGER conversation_message_actions_immutable_route
BEFORE UPDATE ON conversation_message_actions
WHEN NEW.project_id<>OLD.project_id OR NEW.thread_id<>OLD.thread_id
  OR NEW.message_id<>OLD.message_id OR NEW.kind<>OLD.kind
  OR NEW.dedup_key<>OLD.dedup_key OR NEW.payload_text<>OLD.payload_text
  OR NEW.payload_sha256<>OLD.payload_sha256 OR NEW.target_actor_id<>OLD.target_actor_id
  OR NEW.sender_principal_id<>OLD.sender_principal_id
  OR NEW.sender_binding_id IS NOT OLD.sender_binding_id
  OR NEW.target_binding_id<>OLD.target_binding_id
  OR NEW.evidence_baseline_store_seq<>OLD.evidence_baseline_store_seq
  OR NEW.evidence_baseline_uncertainty_epoch<>OLD.evidence_baseline_uncertainty_epoch
BEGIN
    SELECT RAISE(ABORT,'conversation message action immutable identity changed');
END;
