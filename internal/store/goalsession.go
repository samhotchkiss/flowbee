package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GoalSession is one registered long-running codex "goal" session (epic-lane
// Phase 1 watchdog, 0025_goal_sessions.sql). Sessions live in tmux, sometimes on a
// remote box over ssh, for hours-to-days; the watcher (internal/watchdog) polls
// each enabled session's pane every 2 minutes, persists what it saw here, and
// self-serves the boring recovery (usage-limit-expired resume) while flagging the
// operator for anything it must not touch on its own (infra breakage, a real
// weekly cap, 3-strikes rate limiting).
type GoalSession struct {
	ID                  string
	Box                 string // '' = local (the control-plane box itself)
	TmuxName            string
	Repo                string
	Note                string
	State               string // pursuing|working|blocked|achieved|unknown|unreachable
	StateDetail         string // watcher-set annotation, e.g. "needs_operator"
	GoalElapsed         string // raw elapsed text off the status line, informational
	BlockedUntil        string // RFC3339; '' = no auto-resume gate
	ResumeAttempts      int
	ResumeWindowStart   string // RFC3339 start of the current hourly rate-limit window
	ConsecutiveFailures int
	LastPaneHash        string
	LastChangeAt        string
	LastCheckedAt       string
	Enabled             bool
	CreatedAt           string
	UpdatedAt           string
}

// ErrGoalSessionNotFound / ErrGoalSessionExists mirror the ErrRepoNotFound
// convention (F9 repos registry) for the goal-session registry.
var (
	ErrGoalSessionNotFound = errors.New("goal session not found")
	ErrGoalSessionExists   = errors.New("goal session already registered")
)

// AddGoalSession registers a new session (`flowbee session add`). Deliberately NOT
// an upsert (unlike RegisterRepo): re-registering an id is almost always an
// operator typo, and silently rebinding an existing session's tmux target could
// point the watcher's `/goal resume` sends at the wrong pane — better to fail loud
// (ErrGoalSessionExists) than mis-drive keystrokes into a stranger's session.
func (s *Store) AddGoalSession(ctx context.Context, g GoalSession, now time.Time) error {
	if g.ID == "" {
		return errors.New("goal session id is required")
	}
	if g.TmuxName == "" {
		return errors.New("tmux_name is required")
	}
	ts := now.Format(rfc3339)
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO goal_sessions
		    (id, box, tmux_name, repo, note, state, state_detail, goal_elapsed,
		     blocked_until, resume_attempts, resume_window_start, consecutive_failures,
		     last_pane_hash, last_change_at, last_checked_at, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'unknown', '', '', '', 0, '', 0, '', '', '', 1, ?, ?)`,
		g.ID, g.Box, g.TmuxName, g.Repo, g.Note, ts, ts)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return ErrGoalSessionExists
		}
		return fmt.Errorf("add goal session %q: %w", g.ID, err)
	}
	return nil
}

// isUniqueConstraintErr is a best-effort sniff of modernc.org/sqlite's constraint
// error text (it doesn't expose a typed sentinel we can errors.Is against).
func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// GetGoalSession returns one session by id. ErrGoalSessionNotFound if absent.
func (s *Store) GetGoalSession(ctx context.Context, id string) (GoalSession, error) {
	return scanGoalSession(s.DB.QueryRowContext(ctx, goalSessionSelect+` WHERE id = ?`, id))
}

const goalSessionSelect = `
	SELECT id, box, tmux_name, repo, note, state, state_detail, goal_elapsed,
	       blocked_until, resume_attempts, resume_window_start, consecutive_failures,
	       last_pane_hash, last_change_at, last_checked_at, enabled, created_at, updated_at
	  FROM goal_sessions`

func scanGoalSession(row rowScanner) (GoalSession, error) {
	var g GoalSession
	var enabled int
	err := row.Scan(&g.ID, &g.Box, &g.TmuxName, &g.Repo, &g.Note, &g.State, &g.StateDetail,
		&g.GoalElapsed, &g.BlockedUntil, &g.ResumeAttempts, &g.ResumeWindowStart,
		&g.ConsecutiveFailures, &g.LastPaneHash, &g.LastChangeAt, &g.LastCheckedAt,
		&enabled, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return GoalSession{}, ErrGoalSessionNotFound
	}
	if err != nil {
		return GoalSession{}, err
	}
	g.Enabled = enabled != 0
	return g, nil
}

// ListGoalSessions returns every registered session ordered by id (stable), for
// `flowbee session list` — including disabled (paused-watch) ones, so the operator
// can see what's parked, not just what's active.
func (s *Store) ListGoalSessions(ctx context.Context) ([]GoalSession, error) {
	return queryGoalSessions(ctx, s.DB, goalSessionSelect+` ORDER BY id`)
}

// ListEnabledGoalSessions returns only enabled sessions, ordered by id — the exact
// set the watcher's per-tick pass iterates. A disabled session is invisible to the
// watcher: no capture, no parse, no keys, ever (the `session pause` contract).
func (s *Store) ListEnabledGoalSessions(ctx context.Context) ([]GoalSession, error) {
	return queryGoalSessions(ctx, s.DB, goalSessionSelect+` WHERE enabled = 1 ORDER BY id`)
}

func queryGoalSessions(ctx context.Context, db *sql.DB, query string) ([]GoalSession, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GoalSession
	for rows.Next() {
		g, err := scanGoalSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RemoveGoalSession deletes a session from the registry (`flowbee session rm`).
// Idempotent-ish: ErrGoalSessionNotFound if it never existed.
func (s *Store) RemoveGoalSession(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM goal_sessions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrGoalSessionNotFound
	}
	return nil
}

// SetGoalSessionEnabled flips the pause/resume-watch flag. Pausing does NOT clear
// prior observation state (state/state_detail/blocked_until survive) — resuming
// watch picks back up rather than starting blind.
func (s *Store) SetGoalSessionEnabled(ctx context.Context, id string, enabled bool, now time.Time) error {
	e := 0
	if enabled {
		e = 1
	}
	res, err := s.DB.ExecContext(ctx,
		`UPDATE goal_sessions SET enabled = ?, updated_at = ? WHERE id = ?`,
		e, now.Format(rfc3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrGoalSessionNotFound
	}
	return nil
}

// UpsertObservation records one watch pass's parse result. last_change_at only
// advances when paneHash differs from the stored last_pane_hash (the WHY behind the
// name: a session sitting on an unchanged pane must not look "freshly active" every
// tick — last_change_at is a genuine liveness signal, not a last-polled timestamp;
// that's last_checked_at, which always advances on a successful capture). A
// successful observation also clears consecutive_failures (the capture worked) —
// but deliberately does NOT touch state_detail, blocked_until, resume_attempts, or
// resume_window_start: those are the WATCHER's classification/rate-limit outputs
// for the 'blocked' branch, set by the dedicated setters below, not by the raw
// parse step.
func (s *Store) UpsertObservation(ctx context.Context, id, paneHash, state, elapsed string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var priorHash string
		err := tx.QueryRowContext(ctx,
			`SELECT last_pane_hash FROM goal_sessions WHERE id = ?`, id).Scan(&priorHash)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGoalSessionNotFound
		}
		if err != nil {
			return err
		}
		ts := now.Format(rfc3339)
		changeClause := ""
		if paneHash != priorHash {
			changeClause = `, last_change_at = '` + ts + `'`
			// (string-built ONLY for the literal timestamp we just formatted — no
			// user input reaches this clause; every other value stays a bound param.)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE goal_sessions
			   SET state = ?, goal_elapsed = ?, last_pane_hash = ?,
			       consecutive_failures = 0, last_checked_at = ?, updated_at = ?`+changeClause+`
			 WHERE id = ?`,
			state, elapsed, paneHash, ts, ts, id)
		return err
	})
}

// RecordCaptureFailure bumps consecutive_failures on an ssh/tmux capture error
// (§ safety: "ssh/capture failure -> consecutive_failures++, state=unreachable
// after 3 consecutive"). Returns the new count so the caller can log without a
// second read. Never touches last_pane_hash/last_change_at — a failed capture saw
// no pane, so there is nothing to hash.
func (s *Store) RecordCaptureFailure(ctx context.Context, id string, now time.Time) (int, error) {
	var failures int
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var cur int
		err := tx.QueryRowContext(ctx,
			`SELECT consecutive_failures FROM goal_sessions WHERE id = ?`, id).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGoalSessionNotFound
		}
		if err != nil {
			return err
		}
		failures = cur + 1
		state := ""
		if failures >= 3 {
			state = `, state = 'unreachable'`
		}
		ts := now.Format(rfc3339)
		_, err = tx.ExecContext(ctx,
			`UPDATE goal_sessions SET consecutive_failures = ?, last_checked_at = ?, updated_at = ?`+state+` WHERE id = ?`,
			failures, ts, ts, id)
		return err
	})
	if err != nil {
		return 0, err
	}
	return failures, nil
}

// SetBlockedUntil records a parsed usage-limit reset deadline: the watcher will not
// attempt an auto-resume until now >= until. detail is the classification
// annotation ("usage_limit" or "usage_limit_weekly").
func (s *Store) SetBlockedUntil(ctx context.Context, id string, until time.Time, detail string, now time.Time) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE goal_sessions
		   SET blocked_until = ?, state_detail = ?, updated_at = ?
		 WHERE id = ?`,
		until.Format(rfc3339), detail, now.Format(rfc3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrGoalSessionNotFound
	}
	return nil
}

// SetNeedsOperator marks a session as needing a human — the NEVER-auto-resume
// branch (infra breakage, or a session that burned its 3-per-hour resume budget).
// detail is a short human-readable reason folded into state_detail.
func (s *Store) SetNeedsOperator(ctx context.Context, id, detail string, now time.Time) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE goal_sessions SET state_detail = ?, updated_at = ? WHERE id = ?`,
		"needs_operator: "+detail, now.Format(rfc3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrGoalSessionNotFound
	}
	return nil
}

// ClearBlock resets state_detail/blocked_until (the session left the blocked state
// on its own — a normal resume, or the operator manually cleared it) and resets the
// resume-attempt rate-limit window, so a session that unblocks itself doesn't carry
// a stale attempt count into its NEXT block days later.
func (s *Store) ClearBlock(ctx context.Context, id string, now time.Time) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE goal_sessions
		   SET state_detail = '', blocked_until = '', resume_attempts = 0, resume_window_start = '', updated_at = ?
		 WHERE id = ?`,
		now.Format(rfc3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrGoalSessionNotFound
	}
	return nil
}

// goalResumeWindow is the rate-limit window: max 3 auto-resume attempts per
// session per hour (persisted — a serve restart mid-thrash must not reset the
// budget back to 0, or a session flapping across a restart could get hammered).
const goalResumeWindow = time.Hour

// goalResumeMaxPerWindow is the 3-strikes cap (§ safety): the 4th attempt in a
// window is refused (allowed=false) and the caller escalates to needs_operator.
const goalResumeMaxPerWindow = 3

// RecordResumeAttempt atomically checks-and-increments the persisted hourly resume
// budget. allowed=false means the caller must NOT send keys this pass (the budget
// is spent for the window) — the caller is expected to then call SetNeedsOperator.
// A window rolls over (resets to 1/allowed) once now is goalResumeWindow past
// resume_window_start (or resume_window_start is unset). Incrementing happens
// BEFORE the send, not after: a send that itself fails (tmux/ssh flake) still
// counts against the budget — otherwise a persistently-unreachable box could be
// retried without limit, which is exactly the thrash this cap exists to prevent.
func (s *Store) RecordResumeAttempt(ctx context.Context, id string, now time.Time) (attempts int, allowed bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var curAttempts int
		var windowStart string
		e := tx.QueryRowContext(ctx,
			`SELECT resume_attempts, resume_window_start FROM goal_sessions WHERE id = ?`, id).
			Scan(&curAttempts, &windowStart)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrGoalSessionNotFound
		}
		if e != nil {
			return e
		}
		freshWindow := true
		if windowStart != "" {
			if ws, perr := time.Parse(rfc3339, windowStart); perr == nil {
				freshWindow = now.Sub(ws) >= goalResumeWindow
			}
		}
		ts := now.Format(rfc3339)
		if freshWindow {
			attempts = 1
			allowed = true
			_, e = tx.ExecContext(ctx,
				`UPDATE goal_sessions SET resume_attempts = 1, resume_window_start = ?, updated_at = ? WHERE id = ?`,
				ts, ts, id)
			return e
		}
		if curAttempts >= goalResumeMaxPerWindow {
			attempts = curAttempts
			allowed = false
			return nil // no write: budget already spent, window unchanged
		}
		attempts = curAttempts + 1
		allowed = true
		_, e = tx.ExecContext(ctx,
			`UPDATE goal_sessions SET resume_attempts = ?, updated_at = ? WHERE id = ?`,
			attempts, ts, id)
		return e
	})
	if err != nil {
		return 0, false, err
	}
	return attempts, allowed, nil
}
