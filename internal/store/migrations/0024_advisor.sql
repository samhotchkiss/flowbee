-- 0024: Rung E — the read-only LLM advisor tier (the last resort before a human).
-- When the deterministic janitor (0023) has EXHAUSTED its mechanical budget on a stall
-- (unblock_attempts at the cap) and the job is still re-stalling, a single-shot, read-only,
-- no-tools model call NOMINATES one action from a closed set {PLAN, CORRECTION, REPROMPT,
-- STOP}. Go re-authorizes: a PLAN/CORRECTION re-arms the job ONCE with the advisor's note
-- injected as fresh-context; STOP (or any parse failure — fail-safe) leaves it parked.
--
--   stuck_hint      — the advisor's short note, injected into the eng_worker lease context
--                     (LeaseContext.StuckHint) so the rebuild re-enters with "here is what
--                     was tried / try this" instead of the polluted transcript. FOLDED from
--                     the janitor_unblocked event payload (projection == Fold).
--   advisor_attempts — how many times the advisor has been consulted for this job. Capped so
--                     a job the model can't rescue converges to a permanent human park.
--                     Projection-ONLY bookkeeping (not folded): a DR rebuild re-consulting
--                     once more is bounded and safe.
--   advisor_last_hash — the trigger_hash (job_id:reason:head_sha) of the last consult, so the
--                     advisor is not re-run for the SAME stuck signature (SHA-anchored dedup;
--                     a moved SHA is genuinely new work worth another look). Projection-only.
ALTER TABLE jobs ADD COLUMN stuck_hint       TEXT    NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN advisor_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN advisor_last_hash TEXT   NOT NULL DEFAULT '';
