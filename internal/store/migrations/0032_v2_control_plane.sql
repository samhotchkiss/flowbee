-- 0032: Flowbee v2 durable control-plane primitives.
-- This is additive and flag-off: existing epic/job behavior is unchanged until the
-- epic_review_handoff_v2 runtime gate is enabled. 0029/0030 remain retired gaps.

-- Admission identity is scoped to a project. These columns are additive so legacy
-- callers continue to use the historical slug primary key while v2 callers use a
-- stable admission key inside the AddEpicRun transaction.
ALTER TABLE epics ADD COLUMN project_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE epics ADD COLUMN slug TEXT NOT NULL DEFAULT '';
ALTER TABLE epics ADD COLUMN admission_key TEXT NOT NULL DEFAULT '';
ALTER TABLE epics ADD COLUMN work_intent_id TEXT NOT NULL DEFAULT '';
ALTER TABLE epics ADD COLUMN intent_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE epics ADD COLUMN contract_hash TEXT NOT NULL DEFAULT '';
UPDATE epics SET slug = id WHERE slug = '';
UPDATE epics SET admission_key = 'legacy:' || id WHERE admission_key = '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_epics_project_slug
    ON epics(project_id, slug);
CREATE UNIQUE INDEX IF NOT EXISTS idx_epics_project_admission
    ON epics(project_id, admission_key) WHERE admission_key <> '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_epics_project_intent
    ON epics(project_id, work_intent_id, intent_version)
    WHERE work_intent_id <> '';

ALTER TABLE jobs ADD COLUMN workflow_domain TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE jobs ADD COLUMN epic_delivery_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_epic_v2_delivery
    ON jobs(epic_delivery_id) WHERE workflow_domain='epic_v2' AND epic_delivery_id <> '';

CREATE TABLE IF NOT EXISTS projects (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL DEFAULT 'active',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO projects (id, name) VALUES ('default', 'Default project')
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS epic_deliveries (
    epic_id                    TEXT PRIMARY KEY,
    project_id                 TEXT NOT NULL DEFAULT 'default',
    delivery_repo              TEXT NOT NULL DEFAULT '',
    branch                     TEXT NOT NULL DEFAULT '',
    state                      TEXT NOT NULL DEFAULT 'admitted',
    state_version              INTEGER NOT NULL DEFAULT 0,
    ci_state                   TEXT NOT NULL DEFAULT 'unknown',
    review_required            INTEGER NOT NULL DEFAULT 1,
    review_round               INTEGER NOT NULL DEFAULT 0,
    artifact_version           INTEGER NOT NULL DEFAULT 0,
    head_sha                   TEXT NOT NULL DEFAULT '',
    base_sha                   TEXT NOT NULL DEFAULT '',
    ci_green_observed_at       TEXT NOT NULL DEFAULT '',
    state_entered_at           TEXT NOT NULL DEFAULT (datetime('now')),
    state_due_at               TEXT NOT NULL DEFAULT '',
    fact_progress_at           TEXT NOT NULL DEFAULT (datetime('now')),
    review_started_at          TEXT NOT NULL DEFAULT '',
    last_reviewer_fact_at      TEXT NOT NULL DEFAULT '',
    review_eligible_at         TEXT NOT NULL DEFAULT '',
    dispatch_due_at            TEXT NOT NULL DEFAULT '',
    dispatch_attempted_at      TEXT NOT NULL DEFAULT '',
    reviewed_at                TEXT NOT NULL DEFAULT '',
    review_job_id              TEXT NOT NULL DEFAULT '',
    reviewer_identity          TEXT NOT NULL DEFAULT '',
    reviewer_model_family      TEXT NOT NULL DEFAULT '',
    verdict                    TEXT NOT NULL DEFAULT '',
    verdict_head_sha           TEXT NOT NULL DEFAULT '',
    verdict_base_sha           TEXT NOT NULL DEFAULT '',
    builder_model_family       TEXT NOT NULL DEFAULT '',
    builder_affinity_state     TEXT NOT NULL DEFAULT 'none',
    compute_lease_action_id    TEXT NOT NULL DEFAULT '',
    compute_lease_action_epoch INTEGER NOT NULL DEFAULT 0,
    hold_kind                  TEXT NOT NULL DEFAULT '',
    hold_reason                TEXT NOT NULL DEFAULT '',
    return_state               TEXT NOT NULL DEFAULT '',
    recovery_count             INTEGER NOT NULL DEFAULT 0,
    last_recovered_at          TEXT NOT NULL DEFAULT '',
    alert_pending              INTEGER NOT NULL DEFAULT 0,
    alerted_at                 TEXT NOT NULL DEFAULT '',
    last_error                 TEXT NOT NULL DEFAULT '',
    created_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (epic_id) REFERENCES epics(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_epic_deliveries_project_state
    ON epic_deliveries(project_id, state);
CREATE INDEX IF NOT EXISTS idx_epic_deliveries_review
    ON epic_deliveries(state, review_required, ci_state);
CREATE INDEX IF NOT EXISTS idx_epic_deliveries_repo_branch
    ON epic_deliveries(delivery_repo, branch);

INSERT OR IGNORE INTO epic_deliveries
    (epic_id, project_id, delivery_repo, branch, state, review_required,
     builder_model_family, builder_affinity_state, state_entered_at,
     fact_progress_at, created_at, updated_at)
SELECT id, project_id, repo, branch,
       CASE
         WHEN state IN ('launching','running','blocked') THEN 'building'
         WHEN state = 'abandoned' THEN 'abandoned'
         WHEN state IN ('achieved','done') THEN 'awaiting_artifact'
         ELSE 'admitted'
       END,
       CASE WHEN state = 'abandoned' THEN 0 ELSE 1 END,
       builder_model_family,
       CASE
         WHEN state IN ('launching','running','blocked') THEN 'active'
         WHEN state IN ('achieved','done') THEN 'parked'
         WHEN state = 'abandoned' THEN 'abandoned'
         ELSE 'pending'
       END,
       updated_at, updated_at, created_at, updated_at
  FROM epics;

CREATE TABLE IF NOT EXISTS epic_artifacts (
    epic_id                    TEXT PRIMARY KEY,
    project_id                 TEXT NOT NULL DEFAULT 'default',
    repo                       TEXT NOT NULL DEFAULT '',
    branch                     TEXT NOT NULL DEFAULT '',
    pr_number                  INTEGER,
    pr_bound_at                TEXT NOT NULL DEFAULT '',
    head_sha                   TEXT NOT NULL DEFAULT '',
    base_sha                   TEXT NOT NULL DEFAULT '',
    head_updated_at            TEXT NOT NULL DEFAULT '',
    artifact_version           INTEGER NOT NULL DEFAULT 0,
    is_draft                   INTEGER NOT NULL DEFAULT 0,
    pr_open                    INTEGER NOT NULL DEFAULT 0,
    closed_unmerged            INTEGER NOT NULL DEFAULT 0,
    ci_state                   TEXT NOT NULL DEFAULT 'unknown',
    ci_has_real_success        INTEGER NOT NULL DEFAULT 0,
    required_checks_json       TEXT NOT NULL DEFAULT '[]',
    check_contexts_truncated   INTEGER NOT NULL DEFAULT 0,
    ci_green_observed_at       TEXT NOT NULL DEFAULT '',
    mergeable_state            TEXT NOT NULL DEFAULT '',
    merged                     INTEGER NOT NULL DEFAULT 0,
    merge_commit_sha           TEXT NOT NULL DEFAULT '',
    source_observed_at         TEXT NOT NULL DEFAULT '',
    source_updated_at          TEXT NOT NULL DEFAULT '',
    source_watermark           INTEGER NOT NULL DEFAULT 0,
    failure_evidence_json      TEXT NOT NULL DEFAULT '{}',
    created_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (epic_id) REFERENCES epics(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_epic_artifacts_repo_branch
    ON epic_artifacts(repo, branch);
INSERT OR IGNORE INTO epic_artifacts
    (epic_id, project_id, repo, branch, created_at, updated_at)
SELECT id, project_id, repo, branch, created_at, updated_at FROM epics;

CREATE TABLE IF NOT EXISTS epic_actions (
    id                 TEXT PRIMARY KEY,
    project_id         TEXT NOT NULL DEFAULT 'default',
    epic_id            TEXT NOT NULL,
    kind               TEXT NOT NULL,
    state              TEXT NOT NULL DEFAULT 'pending',
    action_epoch       INTEGER NOT NULL DEFAULT 0,
    dedup_key          TEXT NOT NULL,
    payload_json       TEXT NOT NULL DEFAULT '{}',
    payload_sha256     TEXT NOT NULL DEFAULT '',
    -- The observation watermark and uncertainty generation are captured in the
    -- same transaction that creates the immutable action.  Later Driver facts
    -- at or below this point can never acknowledge this action.
    evidence_baseline_store_seq INTEGER NOT NULL DEFAULT 0,
    evidence_baseline_uncertainty_epoch INTEGER NOT NULL DEFAULT 0,
    executor_kind      TEXT NOT NULL DEFAULT 'domain',
    target_role        TEXT NOT NULL DEFAULT '',
    target_host_id     TEXT NOT NULL DEFAULT '',
    target_store_id    TEXT NOT NULL DEFAULT '',
    target_server_id   TEXT NOT NULL DEFAULT '',
    lifecycle_key      TEXT NOT NULL DEFAULT '',
    target_epoch       INTEGER NOT NULL DEFAULT 0,
    profile_id         TEXT NOT NULL DEFAULT '',
    workspace_root_id  TEXT NOT NULL DEFAULT '',
    workspace_relative_path TEXT NOT NULL DEFAULT '',
    lease_id           TEXT NOT NULL DEFAULT '',
    lease_epoch        INTEGER NOT NULL DEFAULT 0,
    sender_session_id  TEXT NOT NULL DEFAULT '',
    sender_agent_run_id TEXT NOT NULL DEFAULT '',
    recipient_session_id TEXT NOT NULL DEFAULT '',
    recipient_pane_instance_id TEXT NOT NULL DEFAULT '',
    recipient_agent_run_id TEXT NOT NULL DEFAULT '',
    grant_id           TEXT NOT NULL DEFAULT '',
    grant_epoch        INTEGER NOT NULL DEFAULT 0,
    grant_expires_at   TEXT NOT NULL DEFAULT '',
    head_sha           TEXT NOT NULL DEFAULT '',
    base_sha           TEXT NOT NULL DEFAULT '',
    attempts           INTEGER NOT NULL DEFAULT 0,
    next_attempt_at    TEXT NOT NULL DEFAULT '',
    claim_owner        TEXT NOT NULL DEFAULT '',
    claim_deadline_at  TEXT NOT NULL DEFAULT '',
    delivery_started_at TEXT NOT NULL DEFAULT '',
    acknowledged_at    TEXT NOT NULL DEFAULT '',
    dead_lettered_at    TEXT NOT NULL DEFAULT '',
    recovery_count      INTEGER NOT NULL DEFAULT 0,
    last_error         TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (epic_id) REFERENCES epic_deliveries(epic_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_epic_actions_live_dedup
    ON epic_actions(dedup_key) WHERE state <> 'cancelled_superseded';
CREATE INDEX IF NOT EXISTS idx_epic_actions_pending
    ON epic_actions(state, next_attempt_at);

-- Durable scheduler-to-Driver identity bindings. A binding is an append-only
-- incarnation: replacing a pane/store/agent run supersedes the old row and mints
-- the next binding_epoch. Review claims resolve these rows in the same transaction
-- that binds the lease; callers may never supply raw pane/session identifiers.
CREATE TABLE IF NOT EXISTS driver_session_bindings (
    binding_id                 TEXT PRIMARY KEY,
    project_id                 TEXT NOT NULL DEFAULT 'default',
    worker_identity            TEXT NOT NULL,
    role                       TEXT NOT NULL,
    binding_epoch              INTEGER NOT NULL,
    state                      TEXT NOT NULL DEFAULT 'active',
    host_id                    TEXT NOT NULL,
    store_id                   TEXT NOT NULL,
    tmux_server_instance_id    TEXT NOT NULL,
    lifecycle_key              TEXT NOT NULL,
    target_epoch               INTEGER NOT NULL,
    profile_id                 TEXT NOT NULL,
    workspace_root_id          TEXT NOT NULL,
    workspace_relative_path    TEXT NOT NULL,
    session_id                 TEXT NOT NULL,
    pane_instance_id           TEXT NOT NULL,
    agent_run_id               TEXT NOT NULL,
    provider                   TEXT NOT NULL DEFAULT '',
    conversation_id            TEXT NOT NULL DEFAULT '',
    observed_at                TEXT NOT NULL,
    superseded_at              TEXT NOT NULL DEFAULT '',
    created_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    UNIQUE(project_id, worker_identity, role, binding_epoch)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_driver_session_bindings_active
    ON driver_session_bindings(project_id, worker_identity, role)
    WHERE state='active';
CREATE INDEX IF NOT EXISTS idx_driver_session_bindings_session
    ON driver_session_bindings(store_id, session_id, pane_instance_id, agent_run_id);

CREATE TABLE IF NOT EXISTS control_alerts (
    id                 TEXT PRIMARY KEY,
    project_id         TEXT NOT NULL DEFAULT 'default',
    epic_id            TEXT,
    kind               TEXT NOT NULL,
    dedup_key          TEXT NOT NULL UNIQUE,
    payload_json       TEXT NOT NULL DEFAULT '{}',
    state              TEXT NOT NULL DEFAULT 'pending',
    alert_epoch        INTEGER NOT NULL DEFAULT 0,
    attempts           INTEGER NOT NULL DEFAULT 0,
    next_attempt_at    TEXT NOT NULL DEFAULT '',
    claim_owner        TEXT NOT NULL DEFAULT '',
    claim_deadline_at  TEXT NOT NULL DEFAULT '',
    acknowledged_at    TEXT NOT NULL DEFAULT '',
    dead_lettered_at   TEXT NOT NULL DEFAULT '',
    last_error         TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (project_id) REFERENCES projects(id),
    FOREIGN KEY (epic_id) REFERENCES epics(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_control_alerts_pending
    ON control_alerts(state, next_attempt_at);

-- Reconciler liveness is control-plane state, not process-local logging.  A
-- replacement process advances run_epoch, fencing a late heartbeat from the old
-- incarnation.  heartbeat_due_at is persisted so the watchdog does not need to
-- reconstruct each loop's cadence after a restart.
CREATE TABLE IF NOT EXISTS reconciler_health (
    name                 TEXT PRIMARY KEY,
    owner                TEXT NOT NULL,
    run_epoch            INTEGER NOT NULL DEFAULT 0,
    state                TEXT NOT NULL DEFAULT 'starting',
    last_started_at      TEXT NOT NULL DEFAULT '',
    last_heartbeat_at    TEXT NOT NULL DEFAULT '',
    heartbeat_due_at     TEXT NOT NULL DEFAULT '',
    last_success_at      TEXT NOT NULL DEFAULT '',
    last_failure_at      TEXT NOT NULL DEFAULT '',
    last_panic_at        TEXT NOT NULL DEFAULT '',
    cursor               TEXT NOT NULL DEFAULT '',
    ledger_seq           INTEGER NOT NULL DEFAULT 0,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    stale_epoch          INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT NOT NULL DEFAULT '',
    updated_at           TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_reconciler_health_due
    ON reconciler_health(state, heartbeat_due_at);

-- A malformed external fact is quarantined independently of loop health.  A
-- resolved fact that later recurs advances poison_epoch and therefore emits a
-- fresh, idempotent alert without losing its prior incident history.
CREATE TABLE IF NOT EXISTS reconciler_poison_facts (
    reconciler_name TEXT NOT NULL,
    fact_key        TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'open',
    poison_epoch    INTEGER NOT NULL DEFAULT 1,
    occurrences     INTEGER NOT NULL DEFAULT 1,
    first_seen_at   TEXT NOT NULL,
    last_seen_at    TEXT NOT NULL,
    resolved_at     TEXT NOT NULL DEFAULT '',
    last_error      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (reconciler_name, fact_key)
);

CREATE TRIGGER IF NOT EXISTS trg_epic_action_dead_letter_alert
AFTER UPDATE OF state ON epic_actions
WHEN NEW.state='dead_letter' AND OLD.state<>'dead_letter'
BEGIN
    INSERT OR IGNORE INTO control_alerts
        (id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
    VALUES
        ('action-dead-letter-' || NEW.id, NEW.project_id, NEW.epic_id,
         'action_dead_letter', 'action_dead_letter:' || NEW.id,
         json_object('action_id',NEW.id,'action_kind',NEW.kind,'epic_id',NEW.epic_id,
                     'head_sha',NEW.head_sha,'last_error',NEW.last_error),
         'pending',NEW.updated_at,NEW.updated_at);
    UPDATE epic_deliveries
       SET alert_pending=1,last_error='action_dead_letter:' || NEW.kind,updated_at=NEW.updated_at
     WHERE epic_id=NEW.epic_id;
END;

CREATE TABLE IF NOT EXISTS driver_grants (
    grant_id                    TEXT NOT NULL,
    project_id                  TEXT NOT NULL DEFAULT 'default',
    action_id                   TEXT NOT NULL DEFAULT '',
    sender_session_id           TEXT NOT NULL,
    sender_agent_run_id         TEXT NOT NULL DEFAULT '',
    recipient_session_id        TEXT NOT NULL,
    recipient_pane_instance_id  TEXT NOT NULL,
    grant_epoch                 INTEGER NOT NULL,
    maximum_payload_bytes       INTEGER NOT NULL DEFAULT 65536,
    allow_draft_stash           INTEGER NOT NULL DEFAULT 0,
    issued_at                   TEXT NOT NULL,
    expires_at                  TEXT NOT NULL,
    revoked_at                  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (grant_id, grant_epoch),
    FOREIGN KEY (project_id) REFERENCES projects(id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_driver_grants_action_epoch
    ON driver_grants(action_id, grant_epoch);

CREATE TABLE IF NOT EXISTS driver_receipts (
    delivery_id                 TEXT PRIMARY KEY,
    action_id                   TEXT NOT NULL,
    grant_id                    TEXT NOT NULL,
    grant_epoch                 INTEGER NOT NULL,
    sender_session_id           TEXT NOT NULL DEFAULT '',
    recipient_session_id        TEXT NOT NULL DEFAULT '',
    recipient_pane_instance_id  TEXT NOT NULL DEFAULT '',
    payload_sha256               TEXT NOT NULL,
    status                      TEXT NOT NULL,
    compatibility_code          INTEGER NOT NULL DEFAULT 0,
    diagnostic_code             TEXT NOT NULL DEFAULT '',
    created_at                  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(action_id, grant_epoch)
);

CREATE TABLE IF NOT EXISTS driver_lifecycle_receipts (
    lifecycle_receipt_id TEXT PRIMARY KEY,
    action_id            TEXT NOT NULL UNIQUE,
    action_epoch         INTEGER NOT NULL,
    operation            TEXT NOT NULL,
    lifecycle_key        TEXT NOT NULL,
    target_epoch         INTEGER NOT NULL,
    lease_id             TEXT NOT NULL,
    lease_epoch          INTEGER NOT NULL,
    status               TEXT NOT NULL,
    identity_before_json TEXT NOT NULL DEFAULT '{}',
    identity_after_json  TEXT NOT NULL DEFAULT '{}',
    absence_observed_at  TEXT NOT NULL DEFAULT '',
    diagnostic_code      TEXT NOT NULL DEFAULT '',
    created_at           TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at           TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (action_id) REFERENCES epic_actions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_driver_lifecycle_receipts_action
    ON driver_lifecycle_receipts(action_id, action_epoch);

CREATE TABLE IF NOT EXISTS driver_observation_cursors (
    store_id          TEXT PRIMARY KEY,
    instance_ref      TEXT NOT NULL,
    cursor            TEXT NOT NULL DEFAULT '',
    high_store_seq    INTEGER NOT NULL DEFAULT 0,
    uncertainty_epoch INTEGER NOT NULL DEFAULT 0,
    last_event_id     TEXT NOT NULL DEFAULT '',
    active            INTEGER NOT NULL DEFAULT 1,
    reset_at          TEXT NOT NULL DEFAULT '',
    updated_at        TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_driver_observation_cursor_instance
    ON driver_observation_cursors(instance_ref, active);
CREATE UNIQUE INDEX IF NOT EXISTS idx_driver_observation_one_active_cursor
    ON driver_observation_cursors(instance_ref) WHERE active=1;

-- Flowbee's inventory key is instance_ref. Driver's store_id is the cursor
-- domain. Reusing an inventory key with a new store_id is recorded as a reset;
-- cursors and projections are never stitched across it.
CREATE TABLE IF NOT EXISTS driver_instances (
    instance_ref       TEXT PRIMARY KEY,
    host_id            TEXT NOT NULL,
    store_id           TEXT NOT NULL,
    producer_boot_id   TEXT NOT NULL,
    state              TEXT NOT NULL DEFAULT 'resyncing',
    reset_count        INTEGER NOT NULL DEFAULT 0,
    last_error         TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS driver_instance_events (
    seq                INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_ref       TEXT NOT NULL,
    kind               TEXT NOT NULL,
    old_store_id       TEXT NOT NULL DEFAULT '',
    new_store_id       TEXT NOT NULL DEFAULT '',
    payload_json       TEXT NOT NULL DEFAULT '{}',
    created_at         TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_driver_instance_events_ref_seq
    ON driver_instance_events(instance_ref, seq);

-- Driver events are an append-only transport evidence ledger. They are kept
-- separate from Flowbee control_events so a Driver resync can replace only the
-- derived projection without rewriting product/workflow audit truth.
CREATE TABLE IF NOT EXISTS driver_observation_events (
    store_id           TEXT NOT NULL,
    event_id           TEXT NOT NULL,
    store_seq          INTEGER NOT NULL,
    cursor             TEXT NOT NULL,
    session_seq        INTEGER NOT NULL,
    transition_id      TEXT NOT NULL,
    transition_index   INTEGER NOT NULL,
    transition_count   INTEGER NOT NULL,
    host_id            TEXT NOT NULL,
    session_id         TEXT NOT NULL,
    pane_instance_id   TEXT NOT NULL,
    producer_boot_id   TEXT NOT NULL,
    kind               TEXT NOT NULL,
    observed_at        TEXT NOT NULL,
    source_at          TEXT NOT NULL DEFAULT '',
    historical         INTEGER NOT NULL DEFAULT 0,
    envelope_sha256    TEXT NOT NULL,
    envelope_json      TEXT NOT NULL,
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (store_id, event_id),
    UNIQUE (store_id, store_seq)
);
CREATE INDEX IF NOT EXISTS idx_driver_observation_events_session
    ON driver_observation_events(store_id, session_id, store_seq);

-- A transport receipt and a workflow-stage acknowledgement are deliberately
-- different records.  This table binds an action epoch to the exact live,
-- provider-native event that proved processing.  It contains no prose and is
-- replay-idempotent; the Driver event ledger remains the evidence authority.
CREATE TABLE IF NOT EXISTS driver_action_evidence (
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
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (action_id, action_epoch),
    UNIQUE (action_id, event_id),
    FOREIGN KEY (action_id) REFERENCES epic_actions(id) ON DELETE CASCADE,
    FOREIGN KEY (store_id, event_id)
        REFERENCES driver_observation_events(store_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_driver_action_evidence_event
    ON driver_action_evidence(store_id, event_id, state);

CREATE TABLE IF NOT EXISTS driver_session_projections (
    store_id                  TEXT NOT NULL,
    session_id                TEXT NOT NULL,
    host_id                   TEXT NOT NULL,
    pane_instance_id          TEXT NOT NULL DEFAULT '',
    agent_run_id              TEXT NOT NULL DEFAULT '',
    tmux_server_instance_id   TEXT NOT NULL DEFAULT '',
    provider                  TEXT NOT NULL DEFAULT '',
    conversation_id           TEXT NOT NULL DEFAULT '',
    lifecycle                 TEXT NOT NULL DEFAULT '',
    phase                     TEXT NOT NULL DEFAULT '',
    binding_status            TEXT NOT NULL DEFAULT '',
    binding_epoch             INTEGER NOT NULL DEFAULT 0,
    state_revision            INTEGER NOT NULL DEFAULT 0,
    last_store_seq            INTEGER NOT NULL DEFAULT 0,
    last_event_id             TEXT NOT NULL DEFAULT '',
    as_of_cursor              TEXT NOT NULL DEFAULT '',
    started_at                TEXT NOT NULL DEFAULT '',
    ended_at                  TEXT NOT NULL DEFAULT '',
    end_reason                TEXT NOT NULL DEFAULT '',
    raw_state_json            TEXT NOT NULL DEFAULT '{}',
    source                    TEXT NOT NULL DEFAULT 'events',
    updated_at                TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (store_id, session_id)
);
CREATE INDEX IF NOT EXISTS idx_driver_session_projection_pane
    ON driver_session_projections(store_id, pane_instance_id);
CREATE INDEX IF NOT EXISTS idx_driver_session_projection_agent
    ON driver_session_projections(store_id, agent_run_id);

CREATE TABLE IF NOT EXISTS control_events (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id   TEXT NOT NULL DEFAULT 'default',
    epic_id      TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL,
    from_state   TEXT NOT NULL DEFAULT '',
    to_state     TEXT NOT NULL DEFAULT '',
    state_version INTEGER NOT NULL DEFAULT 0,
    epic_seq     INTEGER NOT NULL DEFAULT 0,
    actor_kind   TEXT NOT NULL DEFAULT 'flowbee',
    actor_id     TEXT NOT NULL DEFAULT '',
    payload_json TEXT NOT NULL DEFAULT '{}',
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (project_id) REFERENCES projects(id)
);
CREATE INDEX IF NOT EXISTS idx_control_events_project_seq
    ON control_events(project_id, seq);
CREATE UNIQUE INDEX IF NOT EXISTS idx_control_events_epic_seq
    ON control_events(epic_id, epic_seq) WHERE epic_id <> '';

-- The pre-v2 dashboard used Unix milliseconds as its digest. Jump the global
-- sequence above every legacy timestamp before any v2 event is appended.
INSERT INTO control_events
    (seq, project_id, epic_id, kind, epic_seq, actor_kind, payload_json, created_at)
VALUES (
    (SELECT COALESCE(MAX(CAST(strftime('%s', updated_at) AS INTEGER) * 1000), 0) + 1000 FROM epics),
    'default', '', 'digest_sequence_seed', 0, 'migration', '{}', datetime('now')
);
INSERT INTO control_events
    (project_id, epic_id, kind, from_state, to_state, state_version, epic_seq,
     actor_kind, payload_json, created_at)
SELECT project_id, id, 'legacy_epic_backfilled', '',
       CASE
         WHEN state IN ('launching','running','blocked') THEN 'building'
         WHEN state = 'abandoned' THEN 'abandoned'
         WHEN state IN ('achieved','done') THEN 'awaiting_artifact'
         ELSE 'admitted'
       END,
       0, 1, 'migration', '{}', updated_at
  FROM epics;
