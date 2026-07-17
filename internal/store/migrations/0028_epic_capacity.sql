-- 0028: account_windows + seats + epic account/context/auth columns — the
-- epic-lane Phase 6 capacity foundation (plan §4.2, §12.4, §15.13; ladder number
-- reserved as 0028_epic_capacity in migrations/LADDER.md).
--
-- WHY three concerns in one migration: they are ONE feature seam — "which account,
-- on which box, with how much headroom, and how full is the running session's
-- context." acctprobe (internal/acctprobe) yields REAL server usage percentages and
-- a durable account identity; this migration is where those land (account_windows),
-- where an operator records that an account is already logged-in-and-usable on a box
-- (seats), and where an epic records the account/seat it was bound to plus the
-- disk-derived runtime facts the digest surfaces (epics.* columns). Phase 6b wires
-- the consolidated supervision ticker onto these tables; this migration only shapes
-- them.
--
-- SUPERSEDES the never-applied 0023_preemptive_usage_budget (guessed budgets, plan
-- §4.1). The "Codex exposes no live %" premise it rested on is obsolete — both
-- providers expose real server percentages on disk. The number jump to 0028 is
-- recorded in LADDER.md's History.

-- ── account_windows ──
-- One row per ACCOUNT (keyed on the durable accountUuid/account_id, NOT the config
-- dir — plan §4.2: two boxes sharing one login fold into ONE bucket, because quota
-- is per-account). The consolidated capacity ticker folds each acctprobe.Result here
-- via store.UpsertAccountLimits. Percentages are the REAL server-reported utilization
-- (0..100); a -1 sentinel means UNKNOWN (an absent window is never synthesized as a
-- real 0%, matching acctprobe's own "absent ≠ zero" invariant). trust_state carries
-- acctprobe's TrustState verbatim; probe_stale is the §12.14 flag the dashboard and
-- the launch/usage_critical gates read so a flaky-ssh stale reading cannot phantom a
-- critical or hard-refuse a launch.
CREATE TABLE IF NOT EXISTS account_windows (
    -- account_key is acctprobe.Identity.AccountKey (claude accountUuid / codex
    -- account_id) — the same string worker_accounts.account_id keys on, so the two
    -- tables join 1:1 and the legacy ceiling gate reads a usage_pct kept in sync here.
    account_key         TEXT PRIMARY KEY,
    provider            TEXT NOT NULL DEFAULT '',      -- claude | codex
    email               TEXT NOT NULL DEFAULT '',
    model_family        TEXT NOT NULL DEFAULT '',      -- the capacity model_family (== provider)
    -- session_pct / weekly_pct are the highest session-window and account-wide weekly
    -- percentages the last routable reading carried. -1 = UNKNOWN (absent window),
    -- distinct from a real 0%.
    session_pct         REAL NOT NULL DEFAULT -1,
    weekly_pct          REAL NOT NULL DEFAULT -1,
    -- windows_json is the AUTHORITATIVE full per-window set carried verbatim from
    -- acctprobe (plan §2.1 + §15.16): a JSON array of {kind,percent,severity,resets_at,
    -- scope}. It is the public read model's `windows[]` (session + weekly_all +
    -- weekly_scoped all in ONE list — a scoped sub-limit is a member with
    -- kind="weekly_scoped"); the scalar session_pct/weekly_pct stay as the
    -- convenience/fallback numbers.
    windows_json        TEXT NOT NULL DEFAULT '[]',
    severity            TEXT NOT NULL DEFAULT 'normal', -- normal | critical (server flag)
    resets_session_at   TEXT NOT NULL DEFAULT '',       -- RFC3339; '' = unknown
    resets_weekly_at    TEXT NOT NULL DEFAULT '',       -- RFC3339; '' = unknown
    -- trust_state is acctprobe.TrustState verbatim: verified | verified_local | stale |
    -- display_only | held. Only verified/verified_local are Routable() (schedulable);
    -- a non-routable reading updates trust_state + probe_stale but NEVER the last
    -- verified percentages (store.UpsertAccountLimits enforces that).
    trust_state         TEXT NOT NULL DEFAULT 'held',
    probe_stale         INTEGER NOT NULL DEFAULT 1,     -- 1 = the reading is stale (§12.14)
    -- fetched_at_ms is the reading's own capture instant in unix millis (0 = unknown),
    -- so consumers can age it independently of reported_at (our fold time).
    fetched_at_ms       INTEGER NOT NULL DEFAULT 0,
    reported_at         TEXT NOT NULL DEFAULT ''        -- RFC3339 time of the last fold
);

-- ── seats ──
-- A SEAT (plan §15.13) = (account, box, agent family, config dir/env): a place where
-- an account is ALREADY logged in and usable. Flowbee NEVER logs in — the human
-- authenticates each account on each box once; this registry records WHERE, and the
-- launch gate provisions an epic session onto a ready seat by injecting its env
-- (CLAUDE_CONFIG_DIR / CODEX_HOME + FLOWBEE_ACCOUNT) at tmux-session creation. The
-- same account on two boxes is two seats sharing ONE account_windows quota bucket.
CREATE TABLE IF NOT EXISTS seats (
    -- id is a deterministic "<box>|<ident>" composite (store.AddSeat mints it; ident
    -- is config_dir for claude, codex_home for codex), so a seat is stably addressable
    -- and re-adding the same box+dir collides on the UNIQUE below instead of duplicating.
    id             TEXT PRIMARY KEY,
    -- box is a registered epic_hosts.name (the ssh destination; '' = the control-plane
    -- box itself). Validated argv-safe at registration (store.AddSeat), same posture as
    -- AddEpicHost — it flows into the launch ladder's ssh/tmux argv.
    box            TEXT NOT NULL DEFAULT '',
    agent_family   TEXT NOT NULL,                  -- claude | codex
    account_key    TEXT NOT NULL DEFAULT '',       -- account_windows.account_key
    config_dir     TEXT NOT NULL DEFAULT '',       -- CLAUDE_CONFIG_DIR (claude seats)
    codex_home     TEXT NOT NULL DEFAULT '',       -- CODEX_HOME (codex seats)
    extra_env_json TEXT NOT NULL DEFAULT '{}',     -- extra KEY=VAL env injected at launch
    enabled        INTEGER NOT NULL DEFAULT 1,
    -- health: ready | limit_critical | auth_dead | unreachable (plan §15.13a). The
    -- staggered capacity ticker probes each seat over ssh and sets this; the launch
    -- gate selects only ready seats.
    health         TEXT NOT NULL DEFAULT 'unreachable',
    health_detail  TEXT NOT NULL DEFAULT '',
    last_probe_at  TEXT NOT NULL DEFAULT '',        -- RFC3339; '' = never probed
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    -- one seat per (box, config dir/codex home): a family always sets exactly one of
    -- config_dir/codex_home (store.AddSeat enforces it), so this triple is the natural
    -- identity — re-registering the same login on the same box is rejected as a dup.
    UNIQUE (box, config_dir, codex_home)
);

CREATE INDEX IF NOT EXISTS idx_seats_family_enabled ON seats (agent_family, enabled);
CREATE INDEX IF NOT EXISTS idx_seats_account ON seats (account_key);

-- ── epics: account / seat / context / auth binding ──
-- The launch gate BINDS an epic to the account+seat it was provisioned on (plan
-- §4.3/§15.13c); the consolidated ticker writes the disk-derived runtime facts the
-- digest surfaces (plan §2.1, §12.4). All default to a neutral empty/unknown value so
-- the ALTERs are safe against every existing row.
ALTER TABLE epics ADD COLUMN account_key          TEXT NOT NULL DEFAULT '';
ALTER TABLE epics ADD COLUMN seat_id              TEXT NOT NULL DEFAULT '';
-- builder_model_family is the family bound at launch from the account/pane resolution
-- (NOT config intent) — it drives the completion-triggered cross-family review handoff
-- (plan §11). Distinct from epics.agent (the launch verb) so a divergence is visible.
ALTER TABLE epics ADD COLUMN builder_model_family TEXT NOT NULL DEFAULT '';
-- context_pct is the disk-derived REMAINING-context percentage (0..100) of the running
-- session (higher = healthier; ctxprobe). -1 = UNKNOWN — the Claude on-disk transcript
-- often lacks the window size, and we NEVER guess it (plan §12.4). A compaction event
-- RAISES this (context freed) and must not read as drift (plan §15.3).
ALTER TABLE epics ADD COLUMN context_pct          REAL NOT NULL DEFAULT -1;
-- pane_state is the last tmuxio.Classify of the session's pane (working|idle_at_prompt|
-- awaiting_input|goal_blocked|unknown); the disk-preferred digest uses it only for what
-- disk cannot see (plan §12.1).
ALTER TABLE epics ADD COLUMN pane_state           TEXT NOT NULL DEFAULT '';
-- auth_state is the NEW distinct auth-death axis (plan §12.4/§12.13): '' | ok |
-- auth_dead. auth_dead routes to a human re-login, NEVER the auto-resume loop.
ALTER TABLE epics ADD COLUMN auth_state           TEXT NOT NULL DEFAULT '';
-- last_commit_at is the RFC3339 time of the newest commit on the epic branch (the
-- ticker reads it via the mirror), an on-task / commit-rate input.
ALTER TABLE epics ADD COLUMN last_commit_at       TEXT NOT NULL DEFAULT '';
-- explainer_path is the per-epic visual explainer file on the branch (plan §15.14);
-- the dashboard serves it sandboxed. '' = none authored yet.
ALTER TABLE epics ADD COLUMN explainer_path       TEXT NOT NULL DEFAULT '';
