-- 0033: Phase 1 typed human decisions.
--
-- A decision is always bound to the exact request version and immutable subject
-- artifact version/hash the human saw. Responses are an append-only audit log;
-- decision_requests is the current inbox projection.

CREATE TABLE decision_requests (
    id                       TEXT PRIMARY KEY,
    project_id               TEXT NOT NULL,
    epic_id                  TEXT,
    delivery_id              TEXT,
    kind                     TEXT NOT NULL CHECK (kind IN
                                 ('question','plan_review','design_review','authorization','exception')),
    title                    TEXT NOT NULL,
    prompt                   TEXT NOT NULL,
    options_json             TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(options_json)),
    response_schema_json     TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(response_schema_json)),
    expected_response_kinds_json TEXT NOT NULL CHECK (json_valid(expected_response_kinds_json)),
    priority                 INTEGER NOT NULL DEFAULT 3 CHECK (priority BETWEEN 1 AND 5),
    due_at                   TEXT NOT NULL DEFAULT '',
    deferred_until           TEXT NOT NULL DEFAULT '',
    defer_condition          TEXT NOT NULL DEFAULT '',
    requested_by             TEXT NOT NULL,
    route_to                 TEXT NOT NULL,
    subject_artifact_ref     TEXT NOT NULL,
    subject_version          INTEGER NOT NULL CHECK (subject_version > 0),
    subject_sha256           TEXT NOT NULL,
    evidence_refs_json       TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(evidence_refs_json)),
    summary                  TEXT NOT NULL DEFAULT '',
    state                    TEXT NOT NULL DEFAULT 'open' CHECK (state IN
                                 ('open','viewed','answered','approved','changes_requested',
                                  'deferred','superseded','cancelled')),
    request_version          INTEGER NOT NULL DEFAULT 1 CHECK (request_version > 0),
    current_response_id      TEXT NOT NULL DEFAULT '',
    superseded_by            TEXT,
    cancellation_reason      TEXT NOT NULL DEFAULT '',
    resolved_at              TEXT NOT NULL DEFAULT '',
    created_at               TEXT NOT NULL,
    updated_at               TEXT NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (epic_id) REFERENCES epics(id) ON DELETE SET NULL,
    FOREIGN KEY (superseded_by) REFERENCES decision_requests(id)
);
CREATE INDEX idx_decision_requests_project_state_priority
    ON decision_requests(project_id, state, priority, created_at);
CREATE INDEX idx_decision_requests_epic
    ON decision_requests(epic_id) WHERE epic_id IS NOT NULL;

CREATE TABLE decision_responses (
    id                       TEXT PRIMARY KEY,
    project_id               TEXT NOT NULL,
    request_id               TEXT NOT NULL,
    request_version          INTEGER NOT NULL CHECK (request_version > 0),
    subject_version          INTEGER NOT NULL CHECK (subject_version > 0),
    subject_sha256           TEXT NOT NULL,
    kind                     TEXT NOT NULL CHECK (kind IN
                                 ('answer','approve','request_changes','defer','deny')),
    structured_value_json    TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(structured_value_json)),
    comment                  TEXT NOT NULL DEFAULT '',
    actor_id                 TEXT NOT NULL,
    authorization_scope      TEXT NOT NULL DEFAULT '',
    defer_until              TEXT NOT NULL DEFAULT '',
    defer_condition          TEXT NOT NULL DEFAULT '',
    downstream_ack_state     TEXT NOT NULL DEFAULT 'pending' CHECK (downstream_ack_state IN
                                 ('pending','acknowledged','failed')),
    audit_ref                TEXT NOT NULL DEFAULT '',
    idempotency_key          TEXT NOT NULL,
    created_at               TEXT NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (request_id) REFERENCES decision_requests(id),
    UNIQUE (project_id, request_id, idempotency_key)
);
CREATE INDEX idx_decision_responses_request_created
    ON decision_responses(project_id, request_id, created_at);

-- The response ledger is evidence, never a mutable projection. A downstream
-- acknowledgement is represented by a later control event rather than rewriting
-- what the human submitted.
CREATE TRIGGER decision_responses_append_only_update
BEFORE UPDATE ON decision_responses
BEGIN
    SELECT RAISE(ABORT, 'decision_responses is append-only');
END;

CREATE TRIGGER decision_responses_append_only_delete
BEFORE DELETE ON decision_responses
BEGIN
    SELECT RAISE(ABORT, 'decision_responses is append-only');
END;
