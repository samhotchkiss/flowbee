-- M8: Liveness MVP (DESIGN §10) — Rung-3 (clock deadlines) + Rung-4 (governor) +
-- minimal Rung-2 (net-diff-convergence-or-abstain) + the two free fast-paths + the
-- two-rung kill rule (I-13). SQLite translation per the project overrides: TEXT
-- RFC3339 instants, INTEGER booleans, the hand-rolled durable-timer table (0003)
-- carries the new River-replacing kinds (lease_deadline_check / phase_deadline_check).
--
-- Liveness threads into the §6 state machine as SUB-STATE on the lease plus a
-- governor counter (stall_revocations already on jobs from 0002) — NOT a new
-- top-level state. Every revocation is transactional with the epoch bump +
-- compensation enqueue, so a crash mid-revoke replays cleanly.

-- ── Rung-3: per-phase SOFT deadline (role/constraint-derived) ──────────────────
-- phase_deadline_at is the soft deadline: crossing it ARMS the warn->cancel ladder
-- (it does not kill outright — a soft-deadline condemnation still needs a second
-- rung). lease_deadline (0002) is the ABSOLUTE cap (the lone unilateral kill).
ALTER TABLE jobs ADD COLUMN phase_deadline_at TEXT;

-- ── Rung-0 / Rung-1: last worker-reported hints (gameable) ─────────────────────
-- The last heartbeat's Rung-0 health enum and Rung-1 class, recorded for the
-- two-rung rule (they can corroborate an un-gameable rung; they never kill alone).
ALTER TABLE jobs ADD COLUMN agent_health   TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN rung1_class    TEXT NOT NULL DEFAULT '';
-- last_heartbeat_at drives worker_unreachable classification (partition != stall,
-- §10.5): heartbeat silence feeds Rung-3's lease clock but does NOT by itself
-- satisfy the stall kill rule.
ALTER TABLE jobs ADD COLUMN last_heartbeat_at TEXT;

-- ── Rung-2: externally-anchored progress oracle (on the reconcile sweep) ───────
-- rung2_last_verdict is the canonical {converging|stalled|abstain} from the last
-- sweep (§10.7). The sliding-window net-diff baseline: the head SHA + the instant
-- when the window started (a branch that hasn't gained a net line of meaningful
-- diff in the window while Rung-1 claims thousands of tokens is the canonical
-- spinning signature). ci_running records a GitHub CI transition: the moment CI is
-- running, "no new diff" is EXPECTED, not stalled -> it EXTENDS Rung-2's tolerance
-- (Guardrail A, §10.4), so a 40-min E2E suite is never counted as a stall.
ALTER TABLE jobs ADD COLUMN rung2_last_verdict     TEXT NOT NULL DEFAULT 'abstain';
ALTER TABLE jobs ADD COLUMN rung2_window_head      TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN rung2_window_started_at TEXT;
ALTER TABLE jobs ADD COLUMN ci_running             INTEGER NOT NULL DEFAULT 0;
