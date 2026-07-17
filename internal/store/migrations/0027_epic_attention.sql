-- 0027: supervisors + attention_items + wip_markers — the epic-lane attention
-- queue and the master (supervisor) registry (epic-lane Phase 5, plan §1 + §9-P5).
--
-- The attention queue is the master's DURABLE MEMORY, not its transcript (plan bet
-- #1): every actionable supervision condition is a typed, deduped, epoch-fenced row
-- a fresh (post-`/clear`) master rediscovers idempotently. It is DELIBERATELY a
-- lighter parallel table, NOT the `jobs` engine (plan §1.3): a jobs row carries a
-- heavy PR lifecycle (spec→review→merge, outbox, minted verdicts) an ephemeral
-- supervision nudge does not have, and the Decide state machine has no transition
-- for "a master typed a steer." What it DOES copy from the jobs engine is the only
-- two properties that earned their keep: epoch fencing (identical ErrStaleEpoch
-- semantics to internal/lease) and ledgering (every create/lease/resolve/escalate
-- appends a job_events row — see internal/store/{attention,supervisor}.go).

-- ── supervisors ──
-- A "master" is a long-lived interactive Claude/Codex tmux session in its OWN pane
-- that registers as a supervised actor and supplies LEASED judgment. It is NOT a
-- worker and NOT a goal_sessions row (plan bet #4): watchdog.Pass iterates
-- ListEnabledGoalSessions and types `/goal resume` into anything it classifies
-- blocked — registering a master there would fire the resume machinery into the
-- master's own pane. It lives here, structurally separate, so it is never swept
-- into watchdog.Pass.
--
-- Registration is an IDEMPOTENT UPSERT keyed on `label` (the opposite of
-- AddGoalSession/AddEpicRun, which fail loud): re-registration is EXPECTED on every
-- `/clear` or restart. The upsert BUMPS `epoch`, fencing every lease the prior
-- incarnation held — a brand-new master and a post-`/clear` master are the same
-- code path (register → read open items → lease → resolve). `id` is the stable
-- opaque master_id returned to the caller; it defaults to `label` on first insert
-- (they coincide today) and is kept across re-registrations so leases and ledger
-- history stay attributable through a `/clear`.
CREATE TABLE IF NOT EXISTS supervisors (
    id                    TEXT PRIMARY KEY,
    label                 TEXT NOT NULL UNIQUE,
    -- epoch is the fence: bumped on every (re-)registration (plan §1.2). A heartbeat
    -- or lease/resolve carrying an older epoch is a superseded incarnation -> revoked.
    epoch                 INTEGER NOT NULL DEFAULT 0,
    -- active | stale | revoked. stale is set by the liveness reaper when the last
    -- heartbeat is older than 3x the heartbeat interval (plan §1.6); revoked is a
    -- deliberate operator retirement (no CLI writes it yet; the column earns its keep
    -- the moment that command exists rather than needing a follow-up migration).
    state                 TEXT NOT NULL DEFAULT 'active',
    -- kind/model_family describe the master's OWN agent family (claude|codex) — used by
    -- the anti-affinity advisory (a master should differ in family from the builder it
    -- corrects) and to resolve control verbs through internal/verbs.
    kind                  TEXT NOT NULL DEFAULT '',
    model_family          TEXT NOT NULL DEFAULT '',
    box                   TEXT NOT NULL DEFAULT '',
    tmux_name             TEXT NOT NULL DEFAULT '',
    repos_json            TEXT NOT NULL DEFAULT '[]',
    last_heartbeat_at     TEXT NOT NULL DEFAULT '',
    -- last_reported_status/at (plan §15.7): the last human-facing update the master
    -- authored. A fresh post-`/clear` master reads this to CONTINUE the thread rather
    -- than re-reporting or contradicting itself. Written by SetSupervisorLastReport.
    last_reported_status  TEXT NOT NULL DEFAULT '',
    last_reported_at      TEXT NOT NULL DEFAULT '',
    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_supervisors_state ON supervisors(state);

-- ── attention_items ──
-- One row per ACTIVE supervision condition. priority is 1..N where LOWER = MORE
-- urgent (1 = drop-everything), matching 0021_priority_lower_is_urgent — the epic
-- taxonomy (plan §1.3) spans 3 (master_absent) .. 40 (epic_finished).
--
-- state machine: open -> leased -> delivering -> awaiting_ack -> resolved. A master
-- LEASES an open item (fenced, one in-flight per epic across masters), BEGINS a
-- delivery (a fenced verified send into the epic pane), records a verdict
-- (strong/weak -> awaiting_ack; failed -> back to open), and the next digest ACKs it
-- (proves the steer was PROCESSED, not merely submitted — plan §12.3) or reopens it.
--
-- dedup_key + the PARTIAL UNIQUE INDEX below enforce ONE active item per condition
-- STRUCTURALLY (plan §1.3 "Dedup discipline"): a re-seen key bumps occurrences/
-- last_seen_at and refreshes evidence, never a second row. When the condition clears,
-- the producer auto-resolves with resolution='cleared'. The open set is thus a PURE
-- FUNCTION of current reality, not of how many ticks fired — the property that makes
-- a fresh master idempotent.
CREATE TABLE IF NOT EXISTS attention_items (
    id                TEXT PRIMARY KEY,
    kind              TEXT NOT NULL,
    epic_id           TEXT NOT NULL DEFAULT '',
    repo              TEXT NOT NULL DEFAULT '',
    priority          INTEGER NOT NULL DEFAULT 20,
    state             TEXT NOT NULL DEFAULT 'open',
    dedup_key         TEXT NOT NULL,
    -- blocking tiers needs_input (plan §15.4): a blocking prompt escalates on a short
    -- (~10m) window, a non-blocking one on a long (15-30m) window. A plain bool kept
    -- on the row (the escalation policy in internal/attention reads it).
    blocking          INTEGER NOT NULL DEFAULT 0,
    -- leased_by is the supervisors.id holding the lease; item_epoch is the item's OWN
    -- monotonic fence (bumped on every lease) so a stale master's resolve/deliver call
    -- is rejected even if it still knows the item id. lease_expires_at gates the reaper.
    leased_by         TEXT NOT NULL DEFAULT '',
    item_epoch        INTEGER NOT NULL DEFAULT 0,
    lease_expires_at  TEXT NOT NULL DEFAULT '',
    -- awaiting_since is the send-and-ack clock (plan §12.3): the instant the row ENTERED
    -- awaiting_ack (a strong/weak verdict, or a crash-stranded delivery recovered as
    -- already-submitted). AckExpired reads it; it is set ONLY on entry to awaiting_ack and
    -- is NEVER touched by an UpsertAttentionItem refresh (which would corrupt the clock).
    awaiting_since    TEXT NOT NULL DEFAULT '',
    -- delivery_key is the client-generated idempotency key bound at BeginDelivery — the
    -- crash-window recovery (plan §1.5) re-captures the pane and matches against it
    -- rather than blindly re-sending.
    delivery_key      TEXT NOT NULL DEFAULT '',
    evidence_json     TEXT NOT NULL DEFAULT '{}',
    detail            TEXT NOT NULL DEFAULT '',
    resolution        TEXT NOT NULL DEFAULT '',
    verdict           TEXT NOT NULL DEFAULT '',
    occurrences       INTEGER NOT NULL DEFAULT 1,
    first_seen_at     TEXT NOT NULL DEFAULT '',
    last_seen_at      TEXT NOT NULL DEFAULT '',
    resolved_at       TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);
-- ONE active item per condition (plan §1.3). The partial predicate is EXACTLY the
-- in-flight state set: a resolved row does NOT block a fresh item when the condition
-- recurs later (that is a NEW occurrence deserving a new lease), but no two active
-- rows for the same dedup_key can ever coexist — the structural backstop behind
-- UpsertAttentionItem's SELECT-then-INSERT (which is itself safe under the store's
-- MaxOpenConns=1 serialization; the index catches any future concurrent path).
CREATE UNIQUE INDEX IF NOT EXISTS idx_attention_active_dedup
    ON attention_items(dedup_key)
 WHERE state IN ('open','leased','delivering','awaiting_ack');
-- ListOpenAttention / LeaseAttention scan and order by state+priority; this index is the
-- matching scan hint. The lease/stranded reapers narrow to state='leased'|'delivering'
-- with a non-empty lease_expires_at (this index's state prefix helps that first cut) and
-- then compare the expiry IN GO (RFC3339Nano does not sort lexically across the
-- fractional-second boundary, so lease_expires_at is deliberately NOT an indexed range
-- key — the candidate set is tiny). The digest joins by epic (idx_attention_epic).
CREATE INDEX IF NOT EXISTS idx_attention_state_prio ON attention_items(state, priority);
CREATE INDEX IF NOT EXISTS idx_attention_epic ON attention_items(epic_id);

-- ── wip_markers ──
-- Master-registered "a fix is already in flight" markers (plan §15.7). These SURVIVE
-- master compaction/`/clear` so a fresh master does NOT re-dispatch a fix subagent it
-- already launched — the one place the "no subagent tracking" gap bites, closed
-- cheaply. Minimal by design: an active marker is one with cleared_at IS NULL.
CREATE TABLE IF NOT EXISTS wip_markers (
    id             TEXT PRIMARY KEY,
    epic_id        TEXT NOT NULL DEFAULT '',
    pr_number      INTEGER,              -- NULLABLE: a marker may bind a PR or just an epic
    label          TEXT NOT NULL DEFAULT '',
    registered_by  TEXT NOT NULL DEFAULT '',  -- supervisors.id
    started_at     TEXT NOT NULL DEFAULT '',
    eta            TEXT NOT NULL DEFAULT '',
    cleared_at     TEXT,                 -- NULLABLE: NULL = still in flight
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_wip_markers_epic ON wip_markers(epic_id);
