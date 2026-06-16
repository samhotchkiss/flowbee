-- F10: CI as a pluggable fact + a `test` job type (DESIGN build-list §B / §F10).
--
-- Two structural additions:
--   1. The `test` job KIND. The jobs.kind / jobs.flow CHECK constraints only
--      permitted ('spec','build'); a `test` job needs kind='test'. SQLite cannot
--      ALTER a CHECK in place, so we rebuild the jobs table with the relaxed CHECK,
--      preserving every column (exact order) and every index. The copy is
--      `INSERT INTO jobs SELECT * FROM jobs_old` — column order is identical, so it
--      is a faithful migration of all M1-M12 + F1-F9 data.
--   2. test_ci_facts: the PLUGGABLE ci_green@sha fact a Flowbee `test` job produces.
--      The merge gate's ci_green@head is satisfied by EITHER reconcile-from-Actions
--      (domain_b_facts.ci_green) OR a green row here bound to the same head_sha.
--
-- SQLite translation per the project overrides: '?' placeholders, TEXT/INTEGER, no
-- TIMESTAMPTZ, partial unique indexes supported.

-- ── (1) relax the kind/flow CHECK to admit 'test' via a table rebuild ──
ALTER TABLE jobs RENAME TO jobs_old;

CREATE TABLE jobs (
    id                  TEXT PRIMARY KEY,
    kind                TEXT NOT NULL CHECK (kind IN ('spec','build','test')),
    flow                TEXT NOT NULL CHECK (flow IN ('spec','build','test')),
    stage               TEXT NOT NULL,
    state               TEXT NOT NULL,
    role                TEXT NOT NULL,
    chat_ref            TEXT,
    spec_ref            TEXT,
    parent_job          TEXT,
    issue_number        INTEGER,
    pr_number           INTEGER,
    base_sha            TEXT,
    head_sha            TEXT,
    spec_content_hash   TEXT,
    spec_version        INTEGER,
    blocked_by          TEXT    NOT NULL DEFAULT '[]',
    priority            INTEGER NOT NULL DEFAULT 0,
    enqueued_at         TEXT    NOT NULL DEFAULT (datetime('now')),
    lease_id            TEXT,
    lease_epoch         INTEGER NOT NULL DEFAULT 0,
    lease_deadline      TEXT,
    lease_hb_due        TEXT,
    bound_identity      TEXT,
    bound_model_family  TEXT,
    bound_lens          TEXT,
    eng_worker_job      TEXT,
    code_reviewer_job   TEXT,
    attempts            INTEGER NOT NULL DEFAULT 0,
    max_attempts        INTEGER NOT NULL DEFAULT 5,
    bounces             INTEGER NOT NULL DEFAULT 0,
    max_bounces         INTEGER NOT NULL DEFAULT 3,
    stall_revocations   INTEGER NOT NULL DEFAULT 0,
    cost_tokens         INTEGER NOT NULL DEFAULT 0,
    cost_ceiling_tokens INTEGER,
    verdict             TEXT,
    job_seq             INTEGER NOT NULL DEFAULT 0,
    updated_at          TEXT    NOT NULL DEFAULT (datetime('now')),
    required_capabilities TEXT NOT NULL DEFAULT '[]',
    builder_identity     TEXT,
    builder_model_family TEXT,
    head_ref TEXT,
    spec_signoff TEXT,
    spec_signoff_hash TEXT,
    reviewer_lens TEXT,
    author_lens TEXT,
    adopted   INTEGER NOT NULL DEFAULT 0,
    opted_in  INTEGER NOT NULL DEFAULT 0,
    phase_deadline_at TEXT,
    agent_health   TEXT NOT NULL DEFAULT '',
    rung1_class    TEXT NOT NULL DEFAULT '',
    last_heartbeat_at TEXT,
    rung2_last_verdict     TEXT NOT NULL DEFAULT 'abstain',
    rung2_window_head      TEXT NOT NULL DEFAULT '',
    rung2_window_started_at TEXT,
    ci_running             INTEGER NOT NULL DEFAULT 0,
    patch_diff            TEXT NOT NULL DEFAULT '',
    declared_blast_radius TEXT NOT NULL DEFAULT '',
    content_result        TEXT NOT NULL DEFAULT '',
    cost_tokens_in   INTEGER NOT NULL DEFAULT 0,
    cost_tokens_out  INTEGER NOT NULL DEFAULT 0,
    cost_micro_usd   INTEGER NOT NULL DEFAULT 0,
    cost_ceiling_micro_usd INTEGER,
    over_budget INTEGER NOT NULL DEFAULT 0,
    flow_id TEXT NOT NULL DEFAULT '',
    escalation_reason TEXT NOT NULL DEFAULT '',
    build_epoch INTEGER NOT NULL DEFAULT 0,
    merge_provenance TEXT NOT NULL DEFAULT '',
    task_text TEXT NOT NULL DEFAULT '',
    spec_text TEXT NOT NULL DEFAULT '',
    acceptance_criteria TEXT NOT NULL DEFAULT '',
    epic_id TEXT NOT NULL DEFAULT '',
    is_epic INTEGER NOT NULL DEFAULT 0,
    epic_reviewed INTEGER NOT NULL DEFAULT 0,
    bound_account TEXT NOT NULL DEFAULT '',
    needs_full_spec INTEGER NOT NULL DEFAULT 0,
    tracking_label_stage TEXT NOT NULL DEFAULT '',
    reservation_paths TEXT NOT NULL DEFAULT '',
    reservation_wide  INTEGER NOT NULL DEFAULT 0,
    stacked_on_pr INTEGER NOT NULL DEFAULT 0,
    conflict_base_sha TEXT NOT NULL DEFAULT '',
    repo TEXT NOT NULL DEFAULT ''
);

INSERT INTO jobs SELECT * FROM jobs_old;
DROP TABLE jobs_old;

-- recreate every index the rebuilt jobs table needs (drop took them with jobs_old).
CREATE INDEX jobs_ready_idx ON jobs (state, priority DESC, enqueued_at) WHERE state = 'ready';
CREATE INDEX jobs_pr_number_idx ON jobs (pr_number) WHERE pr_number IS NOT NULL;
CREATE INDEX jobs_spec_parent_idx ON jobs (parent_job) WHERE parent_job IS NOT NULL;
CREATE INDEX jobs_epic_idx ON jobs (epic_id) WHERE epic_id <> '';
CREATE UNIQUE INDEX one_active_lease_per_job
    ON jobs (id)
 WHERE state IN ('leased','building','code_review','merging',
                 'merge_handoff','spec_authoring','spec_review',
                 'resolving_conflict');
CREATE INDEX jobs_stacked_idx ON jobs (stacked_on_pr) WHERE stacked_on_pr > 0;
CREATE INDEX jobs_repo_pr_idx ON jobs (repo, pr_number) WHERE pr_number > 0;
CREATE INDEX jobs_repo_state_idx ON jobs (repo, state);

-- ── (2) the pluggable ci_green@sha fact a Flowbee `test` job produces ──
-- A passing `test` job records a green fact bound to the build's HEAD sha. The merge
-- gate ORs this (provenance 'flowbee_test') with the reconciled Actions ci_green —
-- so ci_green@head is satisfied by EITHER producer. The SHA binding is the
-- supersession guard: a green bound to a stale head does not satisfy a moved head.
CREATE TABLE test_ci_facts (
    job_id     TEXT    NOT NULL,
    head_sha   TEXT    NOT NULL,
    green      INTEGER NOT NULL DEFAULT 0,
    provenance TEXT    NOT NULL DEFAULT 'flowbee_test',
    -- test_job_id is the id of the `test` job that produced the fact (audit/lineage).
    test_job_id TEXT   NOT NULL DEFAULT '',
    updated_at TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (job_id, head_sha)
);
