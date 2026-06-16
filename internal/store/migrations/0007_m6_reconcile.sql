-- M6: GitHub reconcile-IN + webhook inbox + App-identity budget gauge.
-- reconcile-IN is the ONLY authority for Domain B (§3.1.B, I-1); it writes ONLY
-- Domain-B fact-fields and never a Domain-A field. Webhooks are HINTS: HMAC
-- verified (I-2), deduped on X-GitHub-Delivery, write-ahead to a durable inbox
-- BEFORE acting, then a TARGETED refetch through the same code path as the sweep.
-- SHA-monotonic + terminal-SHA guards (I-3) gate every ingested fact; a head/base
-- SHA move supersedes a SHA-bound verdict and re-arms (I-5, §6.2.4).

-- domain_b_facts gains the monotonic-gating fields. head_updated_at is the
-- GitHub updatedAt high-water-mark for this PR; an ingested fact older than the
-- recorded value is ignored (SHA-monotonic gate, I-3). merge_commit is the
-- terminal Domain-B fact: once set, the job is frozen (terminal-SHA guard, I-3).
ALTER TABLE domain_b_facts ADD COLUMN head_updated_at TEXT NOT NULL DEFAULT '';
ALTER TABLE domain_b_facts ADD COLUMN merge_commit    TEXT NOT NULL DEFAULT '';
ALTER TABLE domain_b_facts ADD COLUMN is_draft        INTEGER NOT NULL DEFAULT 0;

-- jobs gains pr_number-keyed reconcile lookup. pr_number already exists (0002);
-- index it so reconcile-IN can map a swept PR# back to its job(s).
CREATE INDEX IF NOT EXISTS jobs_pr_number_idx ON jobs (pr_number) WHERE pr_number IS NOT NULL;

-- the durable webhook inbox (I-2): every inbound webhook is written here BEFORE
-- it acts, deduped on the GitHub delivery id. A replayed/forged delivery whose id
-- already exists is dropped (dedupe); a fresh one is recorded then triggers a
-- TARGETED refetch — never a direct state change. status tracks crash-replay:
-- 'pending' rows are re-processable after a crash; 'processed' are done.
CREATE TABLE webhook_inbox (
    delivery_id  TEXT PRIMARY KEY,             -- X-GitHub-Delivery (the dedupe key)
    event        TEXT NOT NULL,                -- X-GitHub-Event (pull_request, push, ...)
    pr_number    INTEGER,                      -- parsed target PR# (the refetch target), if any
    received_at  TEXT NOT NULL DEFAULT (datetime('now')),
    status       TEXT NOT NULL DEFAULT 'pending',  -- pending | processed
    detail       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX webhook_inbox_status_idx ON webhook_inbox (status, received_at);

-- reconcile bookkeeping: the delivery high-water-mark (gap detection, §8.1.4) and
-- the single installation token's rate-limit gauge (I-14, §12.6 — one bucket to
-- watch). A single-row table keyed by a constant.
CREATE TABLE reconcile_state (
    id                   INTEGER PRIMARY KEY CHECK (id = 1),
    last_delivery_id     TEXT NOT NULL DEFAULT '',  -- delivery high-water-mark
    last_sweep_at        TEXT NOT NULL DEFAULT '',
    rate_limit_remaining INTEGER NOT NULL DEFAULT 0,
    rate_limit_limit     INTEGER NOT NULL DEFAULT 0,
    rate_limit_reset_at  TEXT NOT NULL DEFAULT ''
);
INSERT INTO reconcile_state (id) VALUES (1);
