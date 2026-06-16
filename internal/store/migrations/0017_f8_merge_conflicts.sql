-- F8: Merge conflicts — blast-radius reservations, the resolve_conflict job, and
-- integrated-head re-review (DESIGN §E / build-list E).
--
-- Three structural additions:
--   1. resolving_conflict joins the active-lease state set (a conflict-resolver
--      worker holds a lease). The one_active_lease_per_job partial unique index
--      MUST list every active-lease state, so we rebuild it including the new one.
--   2. write-set reservation columns: a build's DECLARED blast-radius, folded as
--      the reservation the scheduler honors so two overlapping builds never co-
--      dispatch (the cheapest conflict is the one never created).
--   3. stacked-PR descendant tracking: a job records the parent PR it is stacked on
--      so a parent merge auto-rebases + re-arms its descendants.
--
-- SQLite translation per the project overrides: '?' placeholders, TEXT/INTEGER,
-- no TIMESTAMPTZ, partial unique indexes supported.

-- ── (1) resolving_conflict joins the active-lease index ──
-- SQLite cannot ALTER a partial index predicate in place; drop + recreate. The new
-- predicate MUST equal job.ActiveLeaseStates (now including resolving_conflict).
DROP INDEX IF EXISTS one_active_lease_per_job;
CREATE UNIQUE INDEX one_active_lease_per_job
    ON jobs (id)
 WHERE state IN ('leased','building','code_review','merging',
                 'merge_handoff','spec_authoring','spec_review',
                 'resolving_conflict');

-- ── (2) blast-radius reservation columns ──
-- reservation_paths: the JSON-encoded declared touched-path prefixes a build holds
-- while in flight (folded from declared_blast_radius at dispatch). reservation_wide:
-- 1 when the build declared a coarse (whole-tree) blast-radius that single-flights.
-- An active build (one holding a lease) with these set is a Reservation the
-- scheduler's ReservationFilter honors. NULL/empty = no reservation held.
ALTER TABLE jobs ADD COLUMN reservation_paths TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN reservation_wide  INTEGER NOT NULL DEFAULT 0;

-- ── (3) stacked-PR descendant tracking ──
-- stacked_on_pr: the GitHub PR number this job's branch is stacked atop (its parent
-- in a PR stack). When that parent merges, this job auto-rebases onto the new main
-- and its SHA-bound verdict supersedes (re-arm review + CI). 0 = not stacked.
ALTER TABLE jobs ADD COLUMN stacked_on_pr INTEGER NOT NULL DEFAULT 0;
CREATE INDEX jobs_stacked_idx ON jobs (stacked_on_pr) WHERE stacked_on_pr > 0;

-- conflict bookkeeping: how a resolve_conflict job came to be (the conflicting
-- base it was rebased against) for audit/replay. Empty for non-conflict jobs.
ALTER TABLE jobs ADD COLUMN conflict_base_sha TEXT NOT NULL DEFAULT '';
