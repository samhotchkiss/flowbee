-- F7: board lifecycle — backlog "needs full spec" flag, the user-agent design
-- loop, the yellow `flowbee` umbrella label, and direct-to-GitHub issues adopted
-- mirrored-quiescent (opt-in via the flowbee:adopt label). Pure-Go modernc SQLite
-- dialect: ALTER TABLE ADD COLUMN with defaults, no network, no TIMESTAMPTZ.

-- needs_full_spec marks a `backlog` job that is tracked + visible but carries a
-- "needs full spec" flag (flow-pass §D): it is a future-consideration item that
-- must be SPEC'd before it can be built. A backlog item with needs_full_spec=1 is
-- promoted into the spec flow (spec_authoring); one with needs_full_spec=0 is a
-- ready-to-build item promoted straight into its flow's entry state. Never leased
-- until deliberately promoted either way (the scheduler only claims `ready`).
ALTER TABLE jobs ADD COLUMN needs_full_spec INTEGER NOT NULL DEFAULT 0;

-- tracking_labels_rendered marks whether the yellow `flowbee` umbrella label (+
-- the per-stage label) has been enqueued for an actively-tracked issue, so the
-- project-OUT label render is enqueued exactly once per (issue, stage) — the
-- (job_id, action, head_sha='') outbox dedupe key already collapses re-enqueues,
-- but this lets the runtime cheaply skip already-rendered issues on a sweep.
ALTER TABLE jobs ADD COLUMN tracking_label_stage TEXT NOT NULL DEFAULT '';
