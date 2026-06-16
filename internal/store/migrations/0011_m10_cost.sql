-- M10: Cost metering + ceilings (I-15) + the unified escalation chokepoint
-- (DESIGN §6.7, §12.6.5). Every lease accumulates {tokens_in, tokens_out, $}
-- reported on heartbeat/result; Flowbee rolls them up per-job AND per-flow against
-- an enforced ceiling. Crossing the ceiling ESCALATES (never silently overspends,
-- I-15): the job is revoked to needs_human, the live worker gets a `cancel`
-- directive on its next heartbeat, and project-OUT stamps a `flowbee:over-budget`
-- label. needs_human is the single chokepoint where all four escalation triggers
-- (attempts, bounces, cost, stall) deposit work (§6.7, §12.6.1).
--
-- SQLite translation per the project overrides: INTEGER counters (no BIGINT-only
-- needs); dollars stored as INTEGER MICRO-USD ($1.00 = 1_000_000) so the meter is
-- exact integer arithmetic — never a float ceiling comparison. flow_id groups the
-- spec+build+review jobs of one feature for the per-flow rollup (§12.6.5).

-- ── per-job cost meter (I-15). cost_tokens already exists from 0002 as a coarse
--    total; M10 splits it into in/out + the exact micro-USD meter the ceiling reads.
ALTER TABLE jobs ADD COLUMN cost_tokens_in   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN cost_tokens_out  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN cost_micro_usd   INTEGER NOT NULL DEFAULT 0;

-- ── the enforced per-job dollar ceiling (micro-USD). NULL = no ceiling (the meter
--    still accumulates for the rollup, but never escalates on $).
ALTER TABLE jobs ADD COLUMN cost_ceiling_micro_usd INTEGER;

-- ── over_budget marks a job that crossed its ceiling and was escalated. It drives
--    the `flowbee:over-budget` label rendering and the needs_human view's reason.
ALTER TABLE jobs ADD COLUMN over_budget INTEGER NOT NULL DEFAULT 0;

-- ── flow_id groups every job (spec_author, spec_reviewer, eng_worker,
--    code_reviewer, merger) of one feature so the per-flow rollup answers "what did
--    this feature cost across spec+build+review?" (§12.6.5). NULL/empty falls back
--    to the job's own id (a standalone job is a flow of one).
ALTER TABLE jobs ADD COLUMN flow_id TEXT NOT NULL DEFAULT '';

-- ── escalation_reason records WHY a job sits in needs_human, so the §12.6.1
--    chokepoint view can show all four triggers (attempts | bounces | cost | stall)
--    in one lane. Set on the transition into needs_human; empty otherwise.
ALTER TABLE jobs ADD COLUMN escalation_reason TEXT NOT NULL DEFAULT '';
