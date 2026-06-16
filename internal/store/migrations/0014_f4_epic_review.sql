-- F4: amend-in-place issue-review + needs_design + epic-level review.
--
-- The flow-pass "amend vs bounce" decision: issue-review AMENDS the spec in place
-- (commits the amended spec; never bounces to the user/spec_author) and, when it
-- needs human DESIGN input, flags needs_design (surfaced on GET /v1/needs-input).
-- Issue-review runs ONCE at the EPIC level — a barrier over ALL the epic's issues
-- (scope · coverage · dep-graph · standards) — before any issue fans out.
--
-- epic_id groups the issues of one epic decomposition. An epic job (the barrier)
-- has epic_id = its own id; each child issue points at it via epic_id. Until the
-- epic-level issue-review passes, the children sit in `backlog` (tracked, NOT
-- scheduled); on epic sign-off they fan out (become leasable). Pure-Go modernc
-- SQLite dialect: ALTER TABLE ADD COLUMN with a TEXT default, no network.
ALTER TABLE jobs ADD COLUMN epic_id TEXT NOT NULL DEFAULT '';

-- is_epic flags the epic-barrier job itself (the one issue-review reviews as a
-- whole). 1 = this job IS the epic gate; 0 = an ordinary job/child issue.
ALTER TABLE jobs ADD COLUMN is_epic INTEGER NOT NULL DEFAULT 0;

-- epic_reviewed flags an epic whose epic-level issue-review barrier has passed —
-- the point after which its issues are allowed to fan out (so a re-check is a
-- no-op and the barrier is crossed exactly once).
ALTER TABLE jobs ADD COLUMN epic_reviewed INTEGER NOT NULL DEFAULT 0;

CREATE INDEX jobs_epic_idx ON jobs (epic_id) WHERE epic_id <> '';
