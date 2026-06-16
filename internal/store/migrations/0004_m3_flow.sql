-- M3: flow engine + build-flow code_review GATE. The verdict, bounces, and SHA
-- columns already exist (0002). This migration records the reconciled Domain-B
-- facts a stubbed FactSource serves the gate (the real reconcile-IN sweep lands
-- in M6) and the per-job head_sha the verdict binds to.
--
-- domain_b_facts: the GitHub-owned facts (§3.1.B) keyed by job. In M3 these are
-- seeded directly by tests (a stub FactSource); reconcile-IN becomes the only
-- writer in M6. They are the AUTHORITY the I-9 gate consumes — never the worker's
-- claim. A missing row means "not reconciled yet" (the gate cannot mint).
CREATE TABLE domain_b_facts (
    job_id     TEXT PRIMARY KEY,
    pr_exists  INTEGER NOT NULL DEFAULT 0,   -- 0/1
    pr_number  INTEGER NOT NULL DEFAULT 0,
    head_sha   TEXT    NOT NULL DEFAULT '',
    base_sha   TEXT    NOT NULL DEFAULT '',
    ci_green   INTEGER NOT NULL DEFAULT 0,   -- 0/1; from the CI rollup
    merged     INTEGER NOT NULL DEFAULT 0,   -- 0/1
    updated_at TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- the sibling pointer the §6.3.1 anti-affinity predicate reads (M4 enforces it;
-- M3 populates eng_worker_job when a review job is spawned so the wiring exists).
-- (eng_worker_job / code_reviewer_job columns already exist in 0002.)
