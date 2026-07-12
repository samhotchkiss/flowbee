-- 0025: goal_sessions — the registry + observation state for the goal-session
-- watchdog (epic-lane Phase 1). Two real incidents motivated this: (1) a codex
-- "goal" session on box `buncher` sat silently blocked ~a day on missing `gh` auth
-- with finished work stranded and nobody watching; (2) sessions routinely max out
-- usage limits and just need an operator (or now: the watchdog) to type
-- `/goal resume` once the window resets. This table is the ONE source of truth the
-- watcher (internal/watchdog) reads/writes every pass, and the CLI (`flowbee
-- session ...`) manages as a registry — a session must be explicitly registered
-- here before the watcher will ever touch its tmux pane (SAFETY: the watcher only
-- ever sends keys to sessions in this registry).
--
-- Column notes:
--   id                    — short stable slug, the operator-chosen handle (PK).
--   box                   — ssh host to reach the tmux session on; '' = local (the
--                           control-plane box itself), matching the box='' == local
--                           convention used elsewhere (Repo.DefaultBranch etc.).
--   tmux_name             — the `tmux -t <name>` target on that box.
--   repo / note           — operator-facing context only; never read by the parser.
--   state                 — the LAST parsed status: pursuing|working|blocked|
--                           achieved|unknown|unreachable. unknown/unreachable are
--                           NEVER acted on (unparseable-or-unreachable => do nothing,
--                           per the "ONE small isolated parser, tiny blast radius"
--                           design goal — a TUI format change degrades to inert, not
--                           to a bad action).
--   state_detail          — a short annotation the WATCHER sets on classification
--                           (e.g. "needs_operator", "usage_limit",
--                           "usage_limit_weekly"), distinct from goal_elapsed below.
--   goal_elapsed           — the raw parenthetical/trailing text ParseStatus pulled
--                           off the status line (e.g. "2d 4h 12m", "30m 48s") —
--                           purely informational, for the operator-facing surface.
--   blocked_until          — RFC3339 deadline before which the watcher will NOT
--                           attempt an auto-resume (set when a usage-limit block's
--                           reset time was parsed from scrollback). '' = no gate.
--   resume_attempts /
--   resume_window_start    — the persisted 3-attempts-per-hour rate limit (a crash
--                           or restart must not reset an in-progress hammer/thrash
--                           window back to 0 — hence persisted, not in-memory).
--   consecutive_failures    — bumped on ssh/tmux capture failure; 3 in a row flips
--                           state to 'unreachable' (warn, no action — distinct from
--                           'blocked', which is a real in-band signal from the pane).
--   last_pane_hash          — hash of the last captured pane text; last_change_at
--                           only advances when this hash actually changes, so a
--                           session sitting on an unchanged pane doesn't look
--                           "freshly observed" every 2-minute tick.
--   last_change_at          — last time the pane content genuinely changed (staleness
--                           signal, distinct from last_checked_at below).
--   last_checked_at         — last time the watcher successfully captured this pane
--                           at all (advances every successful pass regardless of
--                           hash change).
--   enabled                 — the pause/resume-watch flag (`flowbee session
--                           pause|resume-watch <id>`); disabled sessions are skipped
--                           entirely by ListEnabled — no capture, no keys, ever.
CREATE TABLE IF NOT EXISTS goal_sessions (
    id                    TEXT PRIMARY KEY,
    box                   TEXT NOT NULL DEFAULT '',
    tmux_name             TEXT NOT NULL,
    repo                  TEXT NOT NULL DEFAULT '',
    note                  TEXT NOT NULL DEFAULT '',
    state                 TEXT NOT NULL DEFAULT 'unknown',
    state_detail          TEXT NOT NULL DEFAULT '',
    goal_elapsed          TEXT NOT NULL DEFAULT '',
    blocked_until         TEXT NOT NULL DEFAULT '',
    resume_attempts       INTEGER NOT NULL DEFAULT 0,
    resume_window_start   TEXT NOT NULL DEFAULT '',
    consecutive_failures  INTEGER NOT NULL DEFAULT 0,
    last_pane_hash        TEXT NOT NULL DEFAULT '',
    last_change_at        TEXT NOT NULL DEFAULT '',
    last_checked_at       TEXT NOT NULL DEFAULT '',
    enabled               INTEGER NOT NULL DEFAULT 1,
    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL
);
