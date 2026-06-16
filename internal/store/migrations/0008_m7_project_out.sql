-- M7: project-OUT outbox + spec flow + ADOPT mode (DESIGN §8.2, §11, §12.7).
--
-- project-OUT is the ONLY writer to GitHub (R4). Each desired side-effect is an
-- outbox row written transactionally with the Domain-A state change that
-- motivated it (§8.2.2), then drained by a SINGLE serialized sender (≤1 in-flight,
-- honors Retry-After). Every row is keyed (job_id, action, head_sha) for
-- idempotent dedupe — a re-send of the same key collapses to one effect.

-- ── the transactional outbox (§8.2) ──
-- status: pending -> sent (drained) | abandoned (SHA moved; stale row voided).
-- The UNIQUE (job_id, action, head_sha) is the idempotency backbone: enqueuing
-- the same action for the same job+SHA twice is a no-op (ON CONFLICT ignore).
CREATE TABLE outbox (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id      TEXT    NOT NULL,
    action      TEXT    NOT NULL,                 -- pulls.create | issues.create | labels.set | checks.create | mergeQueue.enqueue | ...
    head_sha    TEXT    NOT NULL DEFAULT '',      -- the SHA the action binds to ('' for SHA-less spec actions)
    payload     TEXT    NOT NULL DEFAULT '{}',    -- action-specific JSON args
    status      TEXT    NOT NULL DEFAULT 'pending', -- pending | sent | abandoned
    attempts    INTEGER NOT NULL DEFAULT 0,
    enqueued_at TEXT    NOT NULL DEFAULT (datetime('now')),
    sent_at     TEXT,
    UNIQUE (job_id, action, head_sha)
);
CREATE INDEX outbox_pending_idx ON outbox (status, id) WHERE status = 'pending';

-- ── the audit log (§3.3): every GitHub action appears ONCE, keyed identically to
-- the outbox (job_id, action, head_sha). A drained outbox row writes exactly one
-- audit row; the UNIQUE key makes a re-drain idempotent at the audit layer too —
-- so the M7 DONE-WHEN "every GitHub action appears once in the audit log keyed
-- (job_id, action, head_sha)" is structurally guaranteed.
CREATE TABLE audit_log (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id    TEXT    NOT NULL,
    action    TEXT    NOT NULL,
    head_sha  TEXT    NOT NULL DEFAULT '',
    detail    TEXT    NOT NULL DEFAULT '',         -- e.g. the returned PR number / issue number
    acted_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (job_id, action, head_sha)
);

-- ── spec flow lineage (§11) ──
-- A spec_review job points at the spec_author job it reviews via parent_job
-- (already on jobs). spec_signoff records the minted, content-hash-bound spec
-- sign-off (the spec-flow analogue of jobs.verdict). It is Flowbee-authored, never
-- a worker self-report (I-9), bound to spec_content_hash, and SUPERSEDED the
-- instant the spec bytes change (§11.5).
ALTER TABLE jobs ADD COLUMN spec_signoff TEXT;          -- JSON: the minted spec sign-off, or NULL
ALTER TABLE jobs ADD COLUMN spec_signoff_hash TEXT;     -- the spec_content_hash the sign-off binds to
ALTER TABLE jobs ADD COLUMN reviewer_lens TEXT;         -- the spec_reviewer's lens (distinct-lens anti-affinity)
ALTER TABLE jobs ADD COLUMN author_lens TEXT;           -- the spec_author's lens (§5.5 spec term)

-- ── ADOPT mode (§12.7, I-16) ──
-- adopted=1 marks a job imported from a pre-existing GitHub issue/PR on first
-- boot. Such jobs are reconciled (full Domain-B facts) but QUIESCENT: never
-- scheduled, never rendered OUT. project-OUT suppresses every action on an
-- adopted-quiescent job (the explicit §8.2.3 exception). A job leaves quiescent
-- only on deliberate opt-in (watermark or flowbee:adopt label) -> opted_in=1.
ALTER TABLE jobs ADD COLUMN adopted   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN opted_in  INTEGER NOT NULL DEFAULT 0;

CREATE INDEX jobs_spec_parent_idx ON jobs (parent_job) WHERE parent_job IS NOT NULL;
