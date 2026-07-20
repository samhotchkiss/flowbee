-- 0039: immutable Orchestrator epic contracts and automatic admission.

CREATE TABLE work_intent_epic_contracts (
    id                       TEXT PRIMARY KEY,
    project_id               TEXT NOT NULL,
    work_intent_id           TEXT NOT NULL,
    intent_version           INTEGER NOT NULL CHECK (intent_version > 0),
    source_artifact_sha256   TEXT NOT NULL,
    contract_version         INTEGER NOT NULL CHECK (contract_version > 0),
    contract_ref             TEXT NOT NULL,
    contract_sha256          TEXT NOT NULL,
    contract_json            TEXT NOT NULL CHECK (json_valid(contract_json)),
    orchestrator_binding_id  TEXT NOT NULL,
    submission_key           TEXT NOT NULL,
    state                    TEXT NOT NULL DEFAULT 'prepared'
                             CHECK (state IN ('prepared','admitted','cancelled','superseded')),
    admitted_epic_id         TEXT,
    created_at               TEXT NOT NULL,
    admitted_at              TEXT NOT NULL DEFAULT '',
    UNIQUE (project_id, work_intent_id, intent_version),
    UNIQUE (project_id, submission_key),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (work_intent_id) REFERENCES work_intents(id) ON DELETE CASCADE,
    FOREIGN KEY (orchestrator_binding_id) REFERENCES driver_session_bindings(binding_id),
    FOREIGN KEY (admitted_epic_id) REFERENCES epics(id)
);

CREATE TRIGGER work_intent_epic_contracts_immutable_update
BEFORE UPDATE ON work_intent_epic_contracts
WHEN NEW.id<>OLD.id OR NEW.project_id<>OLD.project_id OR
     NEW.work_intent_id<>OLD.work_intent_id OR NEW.intent_version<>OLD.intent_version OR
     NEW.source_artifact_sha256<>OLD.source_artifact_sha256 OR
     NEW.contract_version<>OLD.contract_version OR NEW.contract_ref<>OLD.contract_ref OR
     NEW.contract_sha256<>OLD.contract_sha256 OR NEW.contract_json<>OLD.contract_json OR
     NEW.orchestrator_binding_id<>OLD.orchestrator_binding_id OR
     NEW.submission_key<>OLD.submission_key OR NEW.created_at<>OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'work_intent_epic_contract immutable fields cannot change');
END;

CREATE TRIGGER work_intent_epic_contracts_no_delete
BEFORE DELETE ON work_intent_epic_contracts
BEGIN
    SELECT RAISE(ABORT, 'work_intent_epic_contracts are append-only');
END;

CREATE INDEX idx_work_intent_epic_contracts_state
    ON work_intent_epic_contracts(state, created_at);
