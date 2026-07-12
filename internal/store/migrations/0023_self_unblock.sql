-- 0023: self-unblock janitor state (the auto-exit from the needs_human sink).
-- Today needs_human is a pure sink: ~8 escalation triggers converge there and the ONLY
-- outgoing edge is an operator `flowbee requeue`. The janitor watchdog gives the
-- MECHANICAL escalation reasons (currently `stall`) a fenced, capped, signal-preserving
-- automatic exit, while genuinely-semantic dead-ends (project_out / pr_closed /
-- reviewer_rejections / cost / attempts / bounces / design) stay parked for a human.
--
-- Three new projection columns:
--   unblock_attempts  — how many times the JANITOR (not the operator) has auto-requeued
--                       this job. Distinct from `attempts` (worker build attempts): the
--                       janitor never resets attempts, so a job that is genuinely out of
--                       build budget still escalates normally. Capped so a job that keeps
--                       re-stalling converges back to needs_human instead of looping.
--                       FOLDED from the janitor_unblocked event (projection == Fold).
--   last_progress_sha — the head||base SHA snapshot taken at the moment of the last
--                       auto-unblock. The next pass compares the job's current SHA against
--                       it: a SHA that MOVED means the rebuild made real progress (allow
--                       another unblock); a SHA that is STATIC across attempts is a churn
--                       plateau (stop, leave parked). This is the deterministic anti-thrash
--                       signal — retry-counting alone can't tell a spinning job from a
--                       progressing one. FOLDED from the janitor_unblocked event.
--   unblock_next_at   — a per-job cooldown gate: the janitor will not re-touch this job
--                       until now >= unblock_next_at. Projection-ONLY scheduling hint (like
--                       lease_hb_due / updated_at), NOT folded — a DR rebuild simply lets
--                       the cooldown lapse, which is safe.
ALTER TABLE jobs ADD COLUMN unblock_attempts  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN last_progress_sha TEXT    NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN unblock_next_at   TEXT    NOT NULL DEFAULT '';
