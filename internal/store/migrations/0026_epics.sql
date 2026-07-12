-- 0026: epic_hosts + epics — the registry for the epic lane (epic-lane Phase 2).
-- An "epic" here is a committed markdown spec (epics/YYYY-MM-DD-<slug>.md on a
-- repo's main branch) that hands a coding agent hours-to-days of unattended work
-- on its own branch (epic/<slug>), in its own tmux session, on ONE host. This is a
-- DELIBERATELY DIFFERENT concept from the pre-existing F4 "epic" (0014_f4_epic_review.sql,
-- store.SeedEpic/EpicIssue, POST /v1/epics): that one is a decomposed GOAL fanned out
-- into child issues inside the normal job pipeline; this one is a single long-running
-- AGENT SESSION outside the job pipeline entirely, tracked via the goal-session
-- watchdog (0025_goal_sessions.sql, Phase 1). The word collides; the mechanisms do not.
-- Go identifiers use `EpicRun`/`EpicHost` (not `Epic`) specifically to keep the two
-- apart in code, even though the DB table is named `epics` per the design doc.
--
-- ── epic_hosts ──
-- The placement registry: a host must be `flowbee host add`-ed before an epic can be
-- launched onto it. ONE-BOX-ONE-EPIC is the core placement rule (WHY: a multi-day
-- unattended agent needs the WHOLE box — disk, gh auth, tmux — to itself; sharing a
-- box between two epics risks one epic's preflight/clone/disk-pressure stepping on the
-- other's, and there is no isolation layer here the way a per-lease worktree gives
-- ordinary builds). Occupancy (an ACTIVE epics row with host = this name) is checked
-- at launch time, not enforced by a DB constraint, because "active" depends on the
-- epics.state machine below, not a static flag on this table.
CREATE TABLE IF NOT EXISTS epic_hosts (
    name       TEXT PRIMARY KEY,
    note       TEXT NOT NULL DEFAULT '',
    -- enabled lets an operator retire a host (`flowbee host rm` removes it outright;
    -- enabled is the softer "stop placing NEW epics here" flag some future `host
    -- pause` command can flip — no CLI writes it false yet, but the launch-time
    -- occupancy/eligibility check already honors it, so the column earns its keep
    -- the moment that command exists rather than needing a follow-up migration).
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- ── epics ──
-- One row per launched (or launch-attempted) epic. id is the SLUG parsed off the
-- filename (epics/YYYY-MM-DD-<slug>.md -> <slug>), a single GLOBAL namespace across
-- every managed repo (not repo-scoped) — collisions are possible in theory (two repos
-- authoring a same-day same-slug epic) but the date-prefixed naming convention makes
-- that vanishingly unlikely in practice, and a global id keeps `flowbee epic abandon
-- <id>` a one-argument command instead of needing a repo qualifier.
--
-- state is the LIFECYCLE flowbee itself drives (pending -> launching -> running, then
-- running <-> blocked as ## Status ingestion reports it, and finally achieved/done/
-- abandoned): pending is reserved for a future "register without launching" path (no
-- current command produces it — `flowbee epic start` moves straight to launching);
-- launching covers the preflight+tmux-create window (so a crash mid-launch leaves a
-- VISIBLE half-launched row rather than nothing, though the current runEpicStart
-- rolls that row back on any launch failure rather than leaving it stranded — see
-- cmd/flowbee/epic.go); blocked/achieved/done/abandoned are documented in
-- epics/INSTRUCTIONS.md and this migration's sibling doc contract. This column is
-- DISTINCT from status_state_detail below, which is the raw agent-reported ## Status
-- "State:" text — flowbee's own state machine only ADVANCES off that signal via a
-- small, documented mapping (nextEpicState in store/epicrun.go — EXACT-token
-- matches only, per review M3: a Contains("done") once fired on "abandoned" and
-- released a live epic's reservations), it never just mirrors it.
--
-- scope_json is the JSON array of the epic's frontmatter `scope:` globs — the
-- blast-radius RESERVATION (design doc: "not a suggestion"). It exists as its own
-- column (not folded into some generic spec blob) because the launch-time overlap
-- check (internal/epicspec.ScopeOverlap) reads it on every OTHER active epic in the
-- same repo before a new one is allowed to start — the #1 reason a multi-day epic can
-- silently wreck another: two agents editing overlapping trees for days without either
-- knowing, discovered only at merge time as an unresolvable conflict. Blocking at
-- launch is far cheaper than resolving that days later.
--
-- status_* columns are what the ~2-minute ingestion tick (serve.go) parses off the
-- epic's OWN branch's ## Status section (epics/INSTRUCTIONS.md "Status discipline") —
-- never off main, which stays spec-immutable once triggered. status_checklist_json is
-- a JSON array of {step,checked,text,evidence}. A parse failure on one epic must never
-- block ingestion of any other (§ task brief) — these columns simply keep their prior
-- values when a pass can't parse (e.g. the branch doesn't exist yet in the first few
-- minutes after launch, or the agent wrote a malformed ## Status).
CREATE TABLE IF NOT EXISTS epics (
    id                     TEXT PRIMARY KEY,
    repo                   TEXT NOT NULL,
    file_path              TEXT NOT NULL,
    title                  TEXT NOT NULL DEFAULT '',
    scope_json             TEXT NOT NULL DEFAULT '[]',
    host                   TEXT NOT NULL DEFAULT '',
    branch                 TEXT NOT NULL DEFAULT '',
    tmux_name              TEXT NOT NULL DEFAULT '',
    -- agent is the coding-agent binary/verb launched in the tmux session (frontmatter
    -- `agent:` override, else the "codex" default per epics/INSTRUCTIONS.md) — also the
    -- worker_accounts.model_family key the quota gate reads at launch time.
    agent                  TEXT NOT NULL DEFAULT '',
    state                  TEXT NOT NULL DEFAULT 'pending',
    status_updated_at      TEXT NOT NULL DEFAULT '',
    status_current_step    INTEGER NOT NULL DEFAULT 0,
    status_steps_total     INTEGER NOT NULL DEFAULT 0,
    status_state_detail    TEXT NOT NULL DEFAULT '',
    status_checklist_json  TEXT NOT NULL DEFAULT '[]',
    status_blockers        TEXT NOT NULL DEFAULT '',
    created_at             TEXT NOT NULL,
    launched_at            TEXT NOT NULL DEFAULT '',
    finished_at            TEXT NOT NULL DEFAULT '',
    updated_at             TEXT NOT NULL
);
-- launch-time gates (scope overlap scoped to a repo; host occupancy scoped globally)
-- both start from "every currently-active epic", so both benefit from these indexes;
-- neither is a uniqueness constraint because "active" is a STATE predicate (see above),
-- not a structural one sqlite can enforce declaratively.
CREATE INDEX IF NOT EXISTS idx_epics_repo ON epics(repo);
CREATE INDEX IF NOT EXISTS idx_epics_host ON epics(host);
CREATE INDEX IF NOT EXISTS idx_epics_state ON epics(state);
