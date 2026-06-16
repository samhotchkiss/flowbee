package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/liveness"
)

// Rung2Sweep is the minimal externally-anchored progress oracle (§10.2, §10.8 MVP
// cut: net-diff-convergence-or-abstain + CI-tolerance + the fleet-wide circuit
// breaker). It runs on the reconcile sweep cadence and looks ONLY at evidence
// Flowbee reconciled ITSELF (the head SHA of the build's ref/PR over a sliding
// window) — never a worker self-report. For each active-lease job it computes the
// canonical verdict {converging | stalled | abstain} and persists it
// (rung2_last_verdict) for the next EvaluateLiveness pass to consume.
//
// Rung-2 ABSTAINS when blind (§10.2): a spec-flow job (no SHA), a build job before
// its first ref push (no reconciled head SHA), or a degraded sweep. An abstaining
// Rung-2 contributes NO vote to the two-rung rule.
//
// Returns whether the fleet-wide circuit breaker is tripped: if too many active
// jobs abstain at once (a wholesale reconcile outage), Flowbee stops trusting
// clock-plus-Rung2 combinations and widens deadlines rather than letting Rung-3
// kill into a blind spot (§10.2). The caller passes the breaker into
// EvaluateLiveness.
func (s *Store) Rung2Sweep(ctx context.Context, src FactSource, now time.Time, cfg LivenessConfig) (bool, error) {
	ids, err := s.ActiveLeaseJobs(ctx)
	if err != nil {
		return false, err
	}
	abstaining := 0
	for _, id := range ids {
		verdict, err := s.rung2Evaluate(ctx, src, id, now, cfg)
		if err != nil {
			return false, err
		}
		if verdict == liveness.Rung2Abstain {
			abstaining++
		}
	}
	tripped := false
	if cfg.CircuitBreakerAbstainFraction > 0 && len(ids) > 0 {
		frac := float64(abstaining) / float64(len(ids))
		tripped = frac >= cfg.CircuitBreakerAbstainFraction
	}
	return tripped, nil
}

// rung2Evaluate computes + persists ONE job's Rung-2 verdict over its sliding
// window. The window baseline is (rung2_window_head, rung2_window_started_at): the
// head SHA when the window opened and when. A net change of the reconciled head SHA
// is content-bearing convergence -> reset the window, verdict `converging`. No net
// change for >= Rung2Window -> `stalled`. A CI-running transition (the GitHub-
// recorded suite running) extends the window (Guardrail A, §10.4) so a long E2E is
// never miscounted as "no new diff".
func (s *Store) rung2Evaluate(ctx context.Context, src FactSource, jobID string, now time.Time, cfg LivenessConfig) (liveness.Rung2Verdict, error) {
	var (
		state, windowHead string
		windowStarted     sql.NullString
		ciRunning         int
		kind              string
	)
	err := s.DB.QueryRowContext(ctx, `
		SELECT state, kind, rung2_window_head, rung2_window_started_at, ci_running
		  FROM jobs WHERE id = ?`, jobID).
		Scan(&state, &kind, &windowHead, &windowStarted, &ciRunning)
	if errors.Is(err, sql.ErrNoRows) {
		return liveness.Rung2Abstain, nil
	}
	if err != nil {
		return "", err
	}

	// spec-flow forces abstain (§10.2 spec note): no SHA exists for spec_authoring /
	// spec_review, so Rung-2 always abstains; spec-flow stall detection leans on
	// Rung-3 + Rung-4 alone (BUILD.md §7.1 item 10).
	if job.Kind(kind) == job.KindSpec ||
		job.State(state) == job.StateSpecAuthoring || job.State(state) == job.StateSpecReview {
		return s.persistRung2(ctx, jobID, liveness.Rung2Abstain, windowHead, windowStarted, ciRunning, now)
	}

	// read the reconciled head SHA (Flowbee's own observation, never the worker's).
	facts, ok, err := src.Facts(ctx, jobID)
	if err != nil {
		return "", err
	}
	// blind: a build job before its first reconciled ref push (no PR / no head SHA) —
	// Rung-2 has nothing to observe -> abstain (§10.2).
	if !ok || facts.HeadSHA == "" {
		return s.persistRung2(ctx, jobID, liveness.Rung2Abstain, windowHead, windowStarted, ciRunning, now)
	}

	// CI-running tolerance (§10.4): a green/pending CI rollup that just started counts
	// as "expected, not stalled" — the caller marks ci_running via MarkCIRunning when
	// the sweep observes the transition. While ci_running, the window is held open
	// (extend tolerance): we re-baseline the window start so the diff-quiet period
	// during the suite is not counted as a stall.
	verdict := liveness.Rung2Converging
	switch {
	case windowHead == "" || windowHead != facts.HeadSHA:
		// first observation, or the head SHA MOVED -> net content-bearing convergence.
		// Reset the window to the new baseline.
		return s.openRung2Window(ctx, jobID, facts.HeadSHA, ciRunning, now)
	case ciRunning == 1:
		// CI is running on this exact head: "no new diff" is expected. Extend the
		// window (re-baseline its start) and report converging — the suite IS progress
		// reaching the outside world.
		return s.openRung2Window(ctx, jobID, facts.HeadSHA, ciRunning, now)
	default:
		// same head SHA, no CI running: has the window aged past the tolerance?
		if windowStarted.Valid {
			if t, perr := time.Parse(rfc3339, windowStarted.String); perr == nil {
				if cfg.Rung2Window > 0 && now.Sub(t) >= cfg.Rung2Window {
					verdict = liveness.Rung2Stalled
				}
			}
		}
		return s.persistRung2(ctx, jobID, verdict, windowHead, windowStarted, ciRunning, now)
	}
}

// openRung2Window (re)baselines the sliding window to head at now and records
// `converging`.
func (s *Store) openRung2Window(ctx context.Context, jobID, head string, ciRunning int, now time.Time) (liveness.Rung2Verdict, error) {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE jobs SET rung2_last_verdict = 'converging', rung2_window_head = ?,
		                rung2_window_started_at = ?, updated_at = datetime('now')
		 WHERE id = ?`, head, now.Format(rfc3339), jobID)
	return liveness.Rung2Converging, err
}

// persistRung2 writes the verdict without disturbing the window baseline.
func (s *Store) persistRung2(ctx context.Context, jobID string, v liveness.Rung2Verdict,
	windowHead string, windowStarted sql.NullString, ciRunning int, now time.Time) (liveness.Rung2Verdict, error) {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE jobs SET rung2_last_verdict = ?, updated_at = datetime('now') WHERE id = ?`,
		string(v), jobID)
	return v, err
}

// MarkCIRunning records a GitHub-recorded CI transition for a job's build (§10.4):
// the moment the suite is running, "no new diff" is EXPECTED, not stalled — it
// extends Rung-2's tolerance window. Called by the reconcile sweep when it observes
// a CI rollup transition to running/pending on the job's head. Fenced to the active
// lease epoch is unnecessary (CI state is a Domain-B fact, not a worker claim).
func (s *Store) MarkCIRunning(ctx context.Context, jobID string, running bool, now time.Time) error {
	v := 0
	if running {
		v = 1
	}
	_, err := s.DB.ExecContext(ctx,
		`UPDATE jobs SET ci_running = ?, updated_at = datetime('now') WHERE id = ?`, v, jobID)
	return err
}

// Rung2VerdictFor reads a job's last persisted Rung-2 verdict (for assertions).
func (s *Store) Rung2VerdictFor(ctx context.Context, jobID string) (liveness.Rung2Verdict, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT rung2_last_verdict FROM jobs WHERE id = ?`, jobID).Scan(&v)
	if err != nil {
		return "", err
	}
	return liveness.Rung2Verdict(v), nil
}
