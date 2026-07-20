-- 0036: durable Phase-1 work intents and automatic Orchestrator delivery.
-- The human never performs a separate "send to Flowbee" transition. Once the
-- immutable intent version is defined and every linked typed decision accepts
-- that exact hash/version, the reconciler creates one durable route action.

CREATE TABLE work_intents (
    id                         TEXT PRIMARY KEY,
    project_id                 TEXT NOT NULL,
    source_conversation_id     TEXT NOT NULL DEFAULT '',
    source_message_id          TEXT NOT NULL,
    source_message_version     INTEGER NOT NULL CHECK (source_message_version > 0),
    interactor_incarnation_id  TEXT NOT NULL,
    title                      TEXT NOT NULL,
    summary                    TEXT NOT NULL DEFAULT '',
    artifact_ref               TEXT NOT NULL,
    intent_version             INTEGER NOT NULL CHECK (intent_version > 0),
    artifact_sha256            TEXT NOT NULL,
    definition_complete        INTEGER NOT NULL DEFAULT 0,
    definition_evidence_json   TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(definition_evidence_json)),
    dependency_refs_json       TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(dependency_refs_json)),
    state                      TEXT NOT NULL DEFAULT 'captured' CHECK (state IN
                               ('captured','defining','awaiting_decision','ready_for_orchestrator',
                                'orchestrating','submitting','admitted','cancelled','superseded')),
    state_version              INTEGER NOT NULL DEFAULT 1 CHECK (state_version > 0),
    priority                   INTEGER NOT NULL DEFAULT 3 CHECK (priority BETWEEN 1 AND 5),
    owner_actor_id             TEXT NOT NULL,
    route_to                   TEXT NOT NULL DEFAULT 'orchestrator',
    orchestrator_registration  TEXT NOT NULL DEFAULT '',
    delivery_action_id         TEXT NOT NULL DEFAULT '',
    route_lease_id             TEXT NOT NULL DEFAULT '',
    route_epoch                INTEGER NOT NULL DEFAULT 0,
    route_attempts             INTEGER NOT NULL DEFAULT 0,
    route_due_at               TEXT NOT NULL DEFAULT '',
    route_acknowledged_at      TEXT NOT NULL DEFAULT '',
    epic_contract_ref          TEXT NOT NULL DEFAULT '',
    epic_contract_sha256       TEXT NOT NULL DEFAULT '',
    submission_idempotency_key TEXT NOT NULL,
    admitted_epic_id           TEXT,
    hold_kind                  TEXT NOT NULL DEFAULT '',
    hold_reason                TEXT NOT NULL DEFAULT '',
    next_retry_at              TEXT NOT NULL DEFAULT '',
    superseded_by              TEXT,
    cancellation_reason        TEXT NOT NULL DEFAULT '',
    created_at                 TEXT NOT NULL,
    updated_at                 TEXT NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (admitted_epic_id) REFERENCES epics(id),
    FOREIGN KEY (superseded_by) REFERENCES work_intents(id),
    UNIQUE (project_id, source_message_id, source_message_version),
    UNIQUE (project_id, submission_idempotency_key)
);
CREATE INDEX idx_work_intents_project_state_priority
    ON work_intents(project_id,state,priority,created_at);
CREATE INDEX idx_work_intents_route_due
    ON work_intents(state,route_due_at) WHERE route_due_at<>'';

CREATE TABLE work_intent_decisions (
    work_intent_id TEXT NOT NULL,
    decision_id    TEXT NOT NULL,
    subject_version INTEGER NOT NULL,
    subject_sha256 TEXT NOT NULL,
    required       INTEGER NOT NULL DEFAULT 1,
    created_at     TEXT NOT NULL,
    PRIMARY KEY (work_intent_id,decision_id),
    FOREIGN KEY (work_intent_id) REFERENCES work_intents(id) ON DELETE CASCADE,
    FOREIGN KEY (decision_id) REFERENCES decision_requests(id)
);

CREATE TABLE work_intent_actions (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL,
    work_intent_id      TEXT NOT NULL,
    intent_version      INTEGER NOT NULL,
    kind                TEXT NOT NULL CHECK (kind IN
                        ('deliver_to_orchestrator','submit_epic','notify_interactor')),
    state               TEXT NOT NULL DEFAULT 'pending' CHECK (state IN
                        ('pending','claimed','delivered','acknowledged','uncertain','dead_letter')),
    action_epoch        INTEGER NOT NULL DEFAULT 0,
    dedup_key           TEXT NOT NULL UNIQUE,
    payload_json        TEXT NOT NULL CHECK (json_valid(payload_json)),
    payload_sha256      TEXT NOT NULL,
    target_actor_id     TEXT NOT NULL,
    target_incarnation  TEXT NOT NULL,
    claim_owner         TEXT NOT NULL DEFAULT '',
    claim_deadline_at   TEXT NOT NULL DEFAULT '',
    receipt_ref         TEXT NOT NULL DEFAULT '',
    attempts            INTEGER NOT NULL DEFAULT 0,
    next_attempt_at     TEXT NOT NULL DEFAULT '',
    last_error          TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (work_intent_id) REFERENCES work_intents(id) ON DELETE CASCADE
);
CREATE INDEX idx_work_intent_actions_pending
    ON work_intent_actions(state,next_attempt_at,created_at);
