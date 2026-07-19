-- 0042: crash-safe human decision response delivery to the exact project Interactor.
--
-- decision_responses remains append-only human evidence. This separate durable
-- action ledger binds a response to immutable content and exact Driver sender
-- and recipient incarnations before any grant or terminal mutation occurs.

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
    sender_binding_id                   TEXT NOT NULL,
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
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (request_id) REFERENCES decision_requests(id),
    FOREIGN KEY (response_id) REFERENCES decision_responses(id),
    FOREIGN KEY (sender_binding_id) REFERENCES driver_session_bindings(binding_id),
    FOREIGN KEY (target_binding_id) REFERENCES driver_session_bindings(binding_id)
);
CREATE UNIQUE INDEX idx_decision_response_actions_live_response
    ON decision_response_actions(response_id) WHERE state<>'fenced';
CREATE INDEX idx_decision_response_actions_pending
    ON decision_response_actions(state,next_attempt_at,created_at);
CREATE INDEX idx_decision_response_actions_ack_due
    ON decision_response_actions(state,acknowledgement_due_at)
    WHERE acknowledgement_due_at<>'';

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

-- Mutable transport fields may advance; immutable response, route, and body
-- identity may never be rewritten to point a committed action elsewhere.
CREATE TRIGGER decision_response_actions_immutable_route
BEFORE UPDATE ON decision_response_actions
WHEN NEW.project_id<>OLD.project_id OR NEW.request_id<>OLD.request_id
  OR NEW.response_id<>OLD.response_id OR NEW.kind<>OLD.kind
  OR NEW.dedup_key<>OLD.dedup_key OR NEW.payload_json<>OLD.payload_json
  OR NEW.payload_sha256<>OLD.payload_sha256 OR NEW.target_actor_id<>OLD.target_actor_id
  OR NEW.sender_binding_id<>OLD.sender_binding_id OR NEW.target_binding_id<>OLD.target_binding_id
  OR NEW.evidence_baseline_store_seq<>OLD.evidence_baseline_store_seq
  OR NEW.evidence_baseline_uncertainty_epoch<>OLD.evidence_baseline_uncertainty_epoch
BEGIN
    SELECT RAISE(ABORT,'decision response action immutable identity changed');
END;
