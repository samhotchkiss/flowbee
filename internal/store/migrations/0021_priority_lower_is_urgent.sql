-- 0021: priority is now a 1..10 scale where LOWER = MORE urgent (1 = drop-everything,
-- 5 = the default for any new issue, 10 = nice-to-have whenever there's time). Previously
-- HIGHER meant more urgent with a default of 0. Two changes:
--   (1) Backfill every priority-0 row (the old "unset" default) to the new default 5, so it
--       ranks at the default band instead of as super-urgent (0 < 1 under the new ordering).
--       All non-zero priorities in existing data are on TERMINAL jobs (historical, never
--       rescheduled), so they are left as-is.
--   (2) Flip the ready-claim index to ASC so the scheduler's lower-is-first ordering is
--       index-aligned (actual ranking is done in scheduler.EffectivePriority, which negates
--       the stored priority; the index is the matching scan hint).
UPDATE jobs SET priority = 5 WHERE priority = 0;
DROP INDEX IF EXISTS jobs_ready_idx;
CREATE INDEX jobs_ready_idx ON jobs (state, priority ASC, enqueued_at) WHERE state = 'ready';
