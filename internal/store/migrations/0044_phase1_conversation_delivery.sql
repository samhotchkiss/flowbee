-- 0044: crash-safe human conversation delivery to the exact project Interactor.
--
-- The conversation message is immutable product truth. This table is the
-- independently mutable Driver transport ledger: Flowbee commits an exact
-- payload, exact A->B incarnations, and an evidence watermark before any
-- Driver grant or terminal mutation can occur.

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
    FOREIGN KEY (thread_id) REFERENCES conversation_threads(id) ON DELETE CASCADE,
    FOREIGN KEY (message_id) REFERENCES conversation_messages(id) ON DELETE CASCADE,
    FOREIGN KEY (sender_binding_id) REFERENCES driver_session_bindings(binding_id),
    FOREIGN KEY (target_binding_id) REFERENCES driver_session_bindings(binding_id)
);
CREATE UNIQUE INDEX idx_conversation_message_actions_live_message
    ON conversation_message_actions(message_id) WHERE state<>'fenced';
CREATE INDEX idx_conversation_message_actions_pending
    ON conversation_message_actions(state,next_attempt_at,created_at);
CREATE INDEX idx_conversation_message_actions_ack_due
    ON conversation_message_actions(state,acknowledgement_due_at)
    WHERE acknowledgement_due_at<>'';

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

CREATE TRIGGER conversation_message_actions_immutable_route
BEFORE UPDATE ON conversation_message_actions
WHEN NEW.project_id<>OLD.project_id OR NEW.thread_id<>OLD.thread_id
  OR NEW.message_id<>OLD.message_id OR NEW.kind<>OLD.kind
  OR NEW.dedup_key<>OLD.dedup_key OR NEW.payload_text<>OLD.payload_text
  OR NEW.payload_sha256<>OLD.payload_sha256 OR NEW.target_actor_id<>OLD.target_actor_id
  OR NEW.sender_binding_id<>OLD.sender_binding_id OR NEW.target_binding_id<>OLD.target_binding_id
  OR NEW.evidence_baseline_store_seq<>OLD.evidence_baseline_store_seq
  OR NEW.evidence_baseline_uncertainty_epoch<>OLD.evidence_baseline_uncertainty_epoch
BEGIN
    SELECT RAISE(ABORT,'conversation message action immutable identity changed');
END;
