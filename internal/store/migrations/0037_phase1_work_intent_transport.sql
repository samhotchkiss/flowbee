-- 0037: exact Driver routing and crash-safe acknowledgement for Phase-1 work intents.
--
-- The work-intent obligation exists before an epic does, so it cannot reuse the
-- epic_actions table (whose epic_id is a required foreign key).  These columns
-- bind the already-durable intent action to two immutable Driver binding rows.
-- Runtime code resolves only those binding IDs; it never infers a route from a
-- tmux name, pane number, CWD, PID, provider string, or wall-clock proximity.

ALTER TABLE work_intent_actions ADD COLUMN sender_binding_id TEXT NOT NULL DEFAULT '';
ALTER TABLE work_intent_actions ADD COLUMN evidence_baseline_store_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE work_intent_actions ADD COLUMN evidence_baseline_uncertainty_epoch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE work_intent_actions ADD COLUMN grant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE work_intent_actions ADD COLUMN grant_epoch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE work_intent_actions ADD COLUMN grant_expires_at TEXT NOT NULL DEFAULT '';
ALTER TABLE work_intent_actions ADD COLUMN delivery_started_at TEXT NOT NULL DEFAULT '';
ALTER TABLE work_intent_actions ADD COLUMN acknowledged_at TEXT NOT NULL DEFAULT '';
ALTER TABLE work_intent_actions ADD COLUMN dead_lettered_at TEXT NOT NULL DEFAULT '';
ALTER TABLE work_intent_actions ADD COLUMN recovery_count INTEGER NOT NULL DEFAULT 0;

CREATE TABLE work_intent_action_evidence (
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
    PRIMARY KEY (action_id, action_epoch),
    UNIQUE (action_id, event_id),
    FOREIGN KEY (action_id) REFERENCES work_intent_actions(id) ON DELETE CASCADE,
    FOREIGN KEY (store_id, event_id)
        REFERENCES driver_observation_events(store_id, event_id)
);

CREATE INDEX idx_work_intent_action_evidence_event
    ON work_intent_action_evidence(store_id, event_id, state);
