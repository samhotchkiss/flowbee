// Package watchdog is Phase 1 of the epic-lane upgrade: the goal-session watchdog.
// It polls registered tmux "goal" sessions (long-running codex CLI agents, hours-
// to-days, sometimes on a remote box over ssh), and answers two real incidents from
// production:
//
//  1. A goal on box `buncher` sat silently blocked ~a day on missing `gh` auth —
//     finished work stranded, nobody knew. -> classifyBlocked's infra branch
//     surfaces this as needs_operator instead of leaving it silent.
//  2. Sessions regularly max out usage limits and just need someone to type
//     `/goal resume` once the window resets. -> the auto-resume branch self-serves
//     that, bounded by a persisted 3-per-hour rate limit.
//
// It does NOT adopt PRs, judge code, or make any decision beyond "type this exact
// resume command, or flag a human" — everything downstream of a session going
// `achieved` stays the orchestrator's job.
package watchdog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// AccountUsageReader is the slice of *store.Store the watcher needs for the
// weekly-limit early warning (§ task brief point 5) — reading the EXISTING
// worker_accounts table rather than introducing a parallel usage source. A narrow
// interface (not *store.Store directly) keeps the watcher's dependency small and
// keeps watchdog_test.go from needing a real DB just to exercise the pane/session
// state machine.
type AccountUsageReader interface {
	AllAccountUsage(ctx context.Context) ([]store.AccountUsageRow, error)
}

// SessionStore is the slice of *store.Store the watcher needs for the goal_sessions
// registry (0025_goal_sessions.sql). Narrow interface for the same testing reason.
type SessionStore interface {
	ListEnabledGoalSessions(ctx context.Context) ([]store.GoalSession, error)
	UpsertObservation(ctx context.Context, id, paneHash, state, elapsed string, now time.Time) error
	RecordCaptureFailure(ctx context.Context, id string, now time.Time) (int, error)
	SetBlockedUntil(ctx context.Context, id string, until time.Time, detail string, now time.Time) error
	SetNeedsOperator(ctx context.Context, id, detail string, now time.Time) error
	ClearBlock(ctx context.Context, id string, now time.Time) error
	RecordResumeAttempt(ctx context.Context, id string, now time.Time) (attempts int, allowed bool, err error)
}

// usageCeilingWarnPct is the early-warning threshold (§ task brief point 5): an
// account at/above this usage fraction gets flagged BEFORE the real (~90%) ceiling
// gates dispatch, so the operator has runway to react instead of finding out when
// work stalls.
const usageCeilingWarnPct = 75

// Watcher runs one watch pass over every enabled goal_sessions row: capture pane ->
// parse -> persist -> (if blocked) classify + maybe auto-resume.
type Watcher struct {
	Sessions SessionStore
	Accounts AccountUsageReader
	Runner   Runner
	Logger   *slog.Logger

	// SettleDelay is the pause between sending `/goal resume` and the verification
	// recapture. A zero-delay recapture routinely catches codex MID-REDRAW — an
	// Unknown-parsing half-drawn pane that reads as a false "swallowed Enter"
	// (review MAJOR #2a). New() defaults it to 500ms; tests set 0 to stay fast.
	SettleDelay time.Duration

	// ceilingWarnMu guards lastCeilingWarnAt — Pass is called from a single
	// serve.go goroutine tick-by-tick, so contention is not expected, but the
	// mutex costs nothing and removes any doubt if that ever changes.
	ceilingWarnMu     sync.Mutex
	lastCeilingWarnAt time.Time

	// scrollbackFails counts CONSECUTIVE scrollback-capture failures per session
	// (review hardening #4): blocked-but-scrollback-unreadable means the watcher
	// cannot distinguish infra breakage from a plain resume — so it takes NO action
	// that pass and retries next tick; after 3 consecutive it flags needs_operator.
	// In-memory (not persisted): a serve restart resetting the count merely re-does
	// up to 3 harmless no-action retries — it can never cause an unsafe resume.
	sbFailMu        sync.Mutex
	scrollbackFails map[string]int
}

// New builds a Watcher with the given store/runner. logger defaults to
// slog.Default() if nil.
func New(st *store.Store, runner Runner, logger *slog.Logger) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Watcher{Sessions: st, Accounts: st, Runner: runner, Logger: logger, SettleDelay: 500 * time.Millisecond}
}

// Pass runs one full watch cycle: every enabled session, plus the weekly-limit
// early-warning sweep over worker_accounts. Errors on individual sessions are
// logged and do not abort the pass — one broken session must never blind the
// watcher to every other session (the exact "one job's failure never blocks the
// others" posture used throughout the rest of the codebase).
func (w *Watcher) Pass(ctx context.Context, now time.Time) {
	// this goroutine TYPES KEYSTROKES into live agent sessions and runs inside the
	// control plane — a panic here (a nil pane edge case, a bad IANA name, anything)
	// must never take `flowbee serve` down with it (review hardening #5). Recover,
	// log loudly, skip the pass; the next 2-minute tick starts clean.
	defer func() {
		if r := recover(); r != nil {
			w.Logger.Error("goal-session watchdog: PANIC recovered — pass skipped", "panic", r)
		}
	}()

	w.warnUsageCeilings(ctx, now)

	sessions, err := w.Sessions.ListEnabledGoalSessions(ctx)
	if err != nil {
		w.Logger.Error("goal-session watchdog: list sessions", "err", err)
		return
	}
	for _, s := range sessions {
		w.watchOne(ctx, s, now)
	}
}

// watchOne runs the capture -> parse -> persist -> classify/act sequence for a
// single session. Never panics/propagates — every branch either persists an
// observation or logs and returns.
func (w *Watcher) watchOne(ctx context.Context, s store.GoalSession, now time.Time) {
	out, err := w.Runner.Run(ctx, capturePaneCmd(s.Box, s.TmuxName))
	if err != nil {
		failures, ferr := w.Sessions.RecordCaptureFailure(ctx, s.ID, now)
		if ferr != nil {
			w.Logger.Error("goal-session watchdog: record capture failure", "session", s.ID, "err", ferr)
			return
		}
		if failures >= 3 {
			w.Logger.Warn("goal-session unreachable (3 consecutive capture failures)",
				"session", s.ID, "box", s.Box, "tmux", s.TmuxName, "err", err)
		} else {
			w.Logger.Warn("goal-session capture failed", "session", s.ID, "consecutive_failures", failures, "err", err)
		}
		return
	}

	state, detail := ParseStatus(out)
	hash := paneHash(out)

	if uerr := w.Sessions.UpsertObservation(ctx, s.ID, hash, string(state), detail, now); uerr != nil {
		w.Logger.Error("goal-session watchdog: upsert observation", "session", s.ID, "err", uerr)
		return
	}

	switch state {
	case StateUnknown:
		// NEVER act on unknown — an unparseable pane (format churn, empty pane,
		// mid-transition) must degrade to inert, not to a guess.
		return
	case StateAchieved:
		w.Logger.Info("goal session achieved", "session", s.ID, "elapsed", detail)
		return
	case StatePursuing, StateWorking:
		// actively making progress: clear any stale block bookkeeping so a future
		// block starts its rate-limit window fresh rather than inheriting an old
		// session's spent attempt budget.
		if s.State == string(StateBlocked) || s.StateDetail != "" || s.BlockedUntil != "" {
			if cerr := w.Sessions.ClearBlock(ctx, s.ID, now); cerr != nil {
				w.Logger.Error("goal-session watchdog: clear block", "session", s.ID, "err", cerr)
			}
		}
		return
	case StateBlocked:
		w.handleBlocked(ctx, s, now)
	}
}

// handleBlocked runs the classify-then-act sequence for a session parsed as
// 'blocked'. It re-captures with scrollback (-S -60) so the classifier sees the
// reason text, which usually has already scrolled off the single-line capture by
// the time the status bar updated.
func (w *Watcher) handleBlocked(ctx context.Context, s store.GoalSession, now time.Time) {
	// a still-future blocked_until from a PRIOR pass means we already know when to
	// try again — skip straight through without re-classifying or re-sending.
	if s.BlockedUntil != "" {
		if until, perr := time.Parse(time.RFC3339Nano, s.BlockedUntil); perr == nil && until.After(now) {
			return
		}
	}

	scrollback, err := w.Runner.Run(ctx, captureScrollbackCmd(s.Box, s.TmuxName))
	if err != nil {
		// blocked-but-scrollback-unreadable = NO ACTION this pass (review hardening
		// #4). The earlier posture — classify off "" → blockAutoResume — meant a
		// genuinely infra-broken session got `/goal resume` typed at it whenever the
		// SECOND capture flaked, masking the exact buncher-style incident this
		// watchdog exists to surface. Without the reason text we cannot distinguish
		// infra (never touch) from a plain resume, so the only safe move is none:
		// retry next tick, and flag needs_operator after 3 consecutive misses (the
		// primary capture works, so this isn't the unreachable path — it's its own
		// "half-blind" mode). Not counted in consecutive_failures for that reason.
		fails := w.bumpScrollbackFail(s.ID)
		w.Logger.Warn("goal-session watchdog: scrollback capture failed — no action this pass",
			"session", s.ID, "consecutive_scrollback_failures", fails, "err", err)
		if fails >= 3 {
			if serr := w.Sessions.SetNeedsOperator(ctx, s.ID, "blocked but scrollback unreadable (3 consecutive)", now); serr != nil {
				w.Logger.Error("goal-session watchdog: set needs_operator (scrollback)", "session", s.ID, "err", serr)
			}
		}
		return
	}
	w.resetScrollbackFail(s.ID)

	// resolve `now` into the BOX's timezone before classification (review MAJOR #1):
	// the usage-limit message renders a BOX-local wall clock, and parseResetTime does
	// all its math in now.Location() — this one In() is the entire timezone fix.
	// tz was validated loadable at registration; a load failure here (e.g. tzdata
	// removed since) falls back to serve-local, logged so the drift is visible.
	boxNow := now
	if s.TZ != "" {
		if loc, lerr := time.LoadLocation(s.TZ); lerr == nil {
			boxNow = now.In(loc)
		} else {
			w.Logger.Warn("goal-session watchdog: cannot load session tz — falling back to serve-local",
				"session", s.ID, "tz", s.TZ, "err", lerr)
		}
	}

	class := classifyBlocked(scrollback, boxNow)
	switch class.Kind {
	case blockUsageLimit:
		detail := "usage_limit"
		if class.Weekly {
			detail = "usage_limit_weekly"
		}
		if serr := w.Sessions.SetBlockedUntil(ctx, s.ID, class.ResetAt, detail, now); serr != nil {
			w.Logger.Error("goal-session watchdog: set blocked_until", "session", s.ID, "err", serr)
			return
		}
		// log the WEEKLY case as an immediate, always-fires warning (§ task brief
		// point 5) — a weekly cap is a much bigger deal than a same-day one, and
		// unlike the ceiling sweep below it's gated on a STATE TRANSITION
		// (previous state_detail differed), not a time-based throttle, so it
		// still only fires once per genuinely-new block rather than every tick.
		if class.Weekly && s.StateDetail != detail {
			w.Logger.Warn("⚠️ goal session hit a WEEKLY usage limit — no auto-resume until it resets",
				"session", s.ID, "reset_at", class.ResetAt.Format(time.RFC3339), "reason", class.Reason)
		} else {
			w.Logger.Info("goal session blocked on usage limit, waiting for reset",
				"session", s.ID, "reset_at", class.ResetAt.Format(time.RFC3339), "reason", class.Reason)
		}
	case blockInfra:
		if serr := w.Sessions.SetNeedsOperator(ctx, s.ID, class.Reason, now); serr != nil {
			w.Logger.Error("goal-session watchdog: set needs_operator", "session", s.ID, "err", serr)
			return
		}
		w.Logger.Warn("⚠️ goal session blocked on an infra problem — needs an operator, NOT auto-resuming",
			"session", s.ID, "box", s.Box, "tmux", s.TmuxName, "reason", class.Reason)
	case blockAutoResume:
		w.autoResume(ctx, s, now)
	}
}

// autoResume enforces the persisted 3-per-hour rate limit, then sends the resume
// keystrokes and verifies submission per the observed double-Enter lesson.
func (w *Watcher) autoResume(ctx context.Context, s store.GoalSession, now time.Time) {
	attempts, allowed, err := w.Sessions.RecordResumeAttempt(ctx, s.ID, now)
	if err != nil {
		w.Logger.Error("goal-session watchdog: record resume attempt", "session", s.ID, "err", err)
		return
	}
	if !allowed {
		if serr := w.Sessions.SetNeedsOperator(ctx, s.ID,
			fmt.Sprintf("rate_limited: %d auto-resume attempts in the last hour", attempts), now); serr != nil {
			w.Logger.Error("goal-session watchdog: set needs_operator (rate limit)", "session", s.ID, "err", serr)
			return
		}
		w.Logger.Warn("⚠️ goal session hit the 3-per-hour auto-resume cap — needs an operator",
			"session", s.ID, "attempts", attempts)
		return
	}

	w.Logger.Info("sending /goal resume", "session", s.ID, "box", s.Box, "tmux", s.TmuxName, "attempt", attempts)
	if _, err := w.Runner.Run(ctx, sendResumeCmd(s.Box, s.TmuxName)); err != nil {
		w.Logger.Warn("goal-session watchdog: send /goal resume failed", "session", s.ID, "err", err)
		return
	}

	// Verify submission (the observed double-Enter lesson): the TUI's
	// slash-command menu can swallow the first Enter, leaving "/goal resume"
	// sitting unsubmitted in the input line. Settle first (review MAJOR #2a): a
	// zero-delay recapture routinely catches codex mid-redraw, which reads as a
	// half-drawn/garbage pane and a false "swallowed Enter". Then re-capture and,
	// only on an EXACT input-line match, send the one bare Enter.
	if w.SettleDelay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.SettleDelay):
		}
	}
	pane, verr := w.Runner.Run(ctx, capturePaneCmd(s.Box, s.TmuxName))
	if verr != nil {
		w.Logger.Warn("goal-session watchdog: verify resume submission failed", "session", s.ID, "err", verr)
		return
	}
	if paneShowsUnsubmittedResume(pane) {
		w.Logger.Info("resume command unsubmitted (TUI swallowed the Enter) — sending bare Enter", "session", s.ID)
		if _, err := w.Runner.Run(ctx, sendEnterCmd(s.Box, s.TmuxName)); err != nil {
			w.Logger.Warn("goal-session watchdog: send bare Enter failed", "session", s.ID, "err", err)
		}
	}
}

// paneShowsUnsubmittedResume reports whether the pane's last non-empty line is the
// swallowed-Enter failure mode: "/goal resume" sitting UNSUBMITTED in the input
// line. EXACT match required (review MAJOR #2b) — after stripping the TUI's input
// prompt glyph (`›`/`>`), the trimmed line must equal exactly "/goal resume":
//   - a Contains check misfires on the legitimately blocked status line itself
//     ("Goal blocked (/goal resume)" — codex's own hint text renders the substring),
//   - on a submitted-and-ECHOED transcript line that merely quotes the command,
//   - and worst, it would press Enter under a HUMAN's edited input like
//     "/goal resume && x", submitting keystrokes the watcher never typed.
//
// Anything that is not the exact unsubmitted command → no bare Enter (fail toward
// no keystroke; the next 2-minute pass re-evaluates from scratch).
func paneShowsUnsubmittedResume(pane string) bool {
	line := lastNonEmptyLine(pane)
	line = strings.TrimSpace(strings.TrimLeft(line, "›>")) // strip the input-prompt glyph(s)
	return line == "/goal resume"
}

// bumpScrollbackFail / resetScrollbackFail maintain the per-session consecutive
// scrollback-capture-failure counter (see handleBlocked). Lazily allocated so the
// zero-value Watcher tests construct keeps working.
func (w *Watcher) bumpScrollbackFail(id string) int {
	w.sbFailMu.Lock()
	defer w.sbFailMu.Unlock()
	if w.scrollbackFails == nil {
		w.scrollbackFails = map[string]int{}
	}
	w.scrollbackFails[id]++
	return w.scrollbackFails[id]
}

func (w *Watcher) resetScrollbackFail(id string) {
	w.sbFailMu.Lock()
	defer w.sbFailMu.Unlock()
	if w.scrollbackFails != nil {
		delete(w.scrollbackFails, id)
	}
}

// paneHash hashes the FULL captured pane text (not just the status line) so
// last_change_at reflects any genuine pane activity, not merely a status-line
// transition.
func paneHash(pane string) string {
	sum := sha256.Sum256([]byte(pane))
	return hex.EncodeToString(sum[:])
}

// warnUsageCeilings implements the weekly-limit early warning (§ task brief point
// 5): any worker_accounts row at/above usageCeilingWarnPct gets logged at WARN —
// but throttled to once per hour (not per 2-minute tick), since the underlying
// condition doesn't change tick-to-tick and log spam would bury the signal it's
// meant to surface. The actual OPERATOR-facing surface (`flowbee status` / `session
// list`) reads worker_accounts directly and independently — it is not gated by
// this in-memory throttle, so it always shows the live number even between WARN
// log lines.
func (w *Watcher) warnUsageCeilings(ctx context.Context, now time.Time) {
	w.ceilingWarnMu.Lock()
	due := now.Sub(w.lastCeilingWarnAt) >= time.Hour
	w.ceilingWarnMu.Unlock()
	if !due {
		return
	}

	accounts, err := w.Accounts.AllAccountUsage(ctx)
	if err != nil {
		w.Logger.Error("goal-session watchdog: read worker_accounts", "err", err)
		return
	}
	var hot []store.AccountUsageRow
	for _, a := range accounts {
		if a.UsagePct < usageCeilingWarnPct {
			continue
		}
		// skip STALE gauges (review hardening #6): an account whose box went quiet
		// pins its last (possibly high-water) usage_pct forever — warning on a
		// >24h-old report is noise about capacity nobody is even using. A missing/
		// unparseable reported_at is NOT skipped (fail toward warning: a fresh-but-
		// odd row shouldn't silently vanish from the alert path).
		if a.ReportedAt != "" {
			if reported, perr := time.Parse(time.RFC3339Nano, a.ReportedAt); perr == nil && now.Sub(reported) > 24*time.Hour {
				continue
			}
		}
		hot = append(hot, a)
	}
	if len(hot) == 0 {
		return
	}
	w.ceilingWarnMu.Lock()
	w.lastCeilingWarnAt = now
	w.ceilingWarnMu.Unlock()
	for _, a := range hot {
		w.Logger.Warn("⚠️ account usage approaching ceiling",
			"account", a.AccountID, "model", a.ModelFamily, "usage_pct", a.UsagePct, "ceiling_pct", a.CeilingPct)
	}
}
