-- F9: Multi-repo — one control plane, a SET of repos, a SHARED fleet (build-list F9).
--
-- One Flowbee control plane manages several GitHub repos at once. Jobs/issues/PRs
-- become repo-SCOPED (a PR #1000 in repo A is a different PR from #1000 in repo B),
-- reconcile-IN + project-OUT run PER repo (each repo has its own GitHub coords and
-- its own single-caller loop), but the SCHEDULER and the worker fleet stay GLOBAL:
-- any repo's ready work routes to any capable worker, and cross-repo prioritization
-- is just the existing priority/aging ranking over the union of all repos' ready
-- jobs. Workers stay repo-AGNOSTIC (they advertise capabilities, never a repo).
--
-- SQLite translation per the project overrides: '?' placeholders, TEXT/INTEGER, no
-- TIMESTAMPTZ (TEXT/RFC3339 + datetime('now')), partial unique indexes supported.

-- ── (1) the repos registry ──
-- Each row is one managed repo's GitHub coordinates. `id` is a short stable handle
-- (e.g. "core", "web") used to scope jobs; (owner, repo) are the GitHub coords the
-- per-repo reconcile-IN / project-OUT loops use. default_branch is the integration
-- branch (the project-OUT PR base + the I-8 protection target). active=0 parks a
-- repo (its loops stop; its jobs are no longer scheduled) without deleting history.
CREATE TABLE repos (
    id             TEXT PRIMARY KEY,                      -- short stable handle, repo-scope key
    owner          TEXT NOT NULL,                         -- GitHub owner/org
    repo           TEXT NOT NULL,                         -- GitHub repo name
    default_branch TEXT NOT NULL DEFAULT 'main',          -- integration branch
    active         INTEGER NOT NULL DEFAULT 1,            -- 0 parks the repo (loops + scheduling stop)
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (owner, repo)                                  -- one registry row per GitHub repo
);

-- ── (2) repo-scope every job ──
-- A job belongs to exactly one repo. NULL/'' is the legacy single-repo default (the
-- pre-F9 jobs + any seed that omits a repo), so existing M1–M12 rows and tests keep
-- working unchanged. The scope is used by:
--   * reconcile-IN: JobIDForPRInRepo scopes the swept PR-number -> job mapping per
--     repo, so PR #1000 in two repos never cross-binds.
--   * project-OUT: the per-repo sender drains only its repo's outbox rows.
--   * the global scheduler reads the union across repos (cross-repo prioritization).
ALTER TABLE jobs ADD COLUMN repo TEXT NOT NULL DEFAULT '';

-- the swept-PR -> job lookup is now (repo, pr_number); index it so the per-repo
-- reconcile sweep's binding stays a point lookup at scale.
CREATE INDEX jobs_repo_pr_idx ON jobs (repo, pr_number) WHERE pr_number > 0;
CREATE INDEX jobs_repo_state_idx ON jobs (repo, state);
