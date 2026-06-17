package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/engine"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/liveness"
)

// LivenessConfig carries the static, constraint-derived liveness budgets (§10.8
// MVP cut: static budgets, no adaptive priors). The runtime sets these from config.
type LivenessConfig struct {
	// PhaseBudget is the per-phase SOFT deadline (Rung-3): role/constraint-derived in
	// production; a single static budget in the MVP. Crossing it arms the warn->kill
	// ladder but needs a second rung.
	PhaseBudget time.Duration
	// AbsoluteCap is the absolute lease cap (Rung-3): the un-gameable floor. A
	// stuck-but-heartbeating worker can NEVER hold a job past it.
	AbsoluteCap time.Duration
	// Rung2Window is the sliding window over which net-diff convergence is judged: a
	// branch with no net meaningful diff for this long (while Rung-1 claims activity)
	// is `stalled`. A CI-running transition extends this window (§10.4).
	Rung2Window time.Duration
	// GovernorCeiling is the Rung-4 anti-thrash ceiling on stall_revocations: a job
	// killed-and-resumed this many times sticks in needs_human rather than re-arming.
	// Distinct from max_attempts / max_bounces (§10.7).
	GovernorCeiling int
	// CircuitBreakerAbstainFraction trips the fleet-wide Rung-2 circuit breaker when
	// this fraction (or more) of active jobs have Rung-2 abstaining at once (a
	// wholesale reconcile outage) — Flowbee then widens deadlines rather than letting
	// Rung-3 kill into a blind spot (§10.2). Zero disables the breaker.
	CircuitBreakerAbstainFraction float64
	// HeartbeatReapAfter is how long a leased job may go WITHOUT a heartbeat before the
	// worker is presumed dead and the lease reaped (a unilateral, clock-truth kill —
	// Rung3.HeartbeatStale). A cleanly-crashed worker reports no unhealthy hint, so
	// without this it waits the full AbsoluteCap (~20m) to recover. Must be safely above
	// the worker heartbeat interval + worktree-setup time (the worker heartbeats every
	// ~60s during the agent run, and setup precedes the first beat). Zero disables it
	// (only the absolute cap reaps a silent worker). A few minutes is the sweet spot:
	// fast crash recovery without false-reaping a live-but-quiet setup phase.
	HeartbeatReapAfter time.Duration
}

// timer kinds for the M8 liveness checks (the River-replacing durable timers,
// §3.5: idempotent + epoch-guarded — a stale timer is a no-op).
const (
	TimerLeaseDeadline = "lease_deadline_check" // absolute cap (Rung-3) — unilateral revoke
	TimerPhaseDeadline = "phase_deadline_check" // soft deadline (Rung-3) — arms the ladder
)

// ArmLeaseLivenessTimers arms the phase-soft + absolute-cap deadline checks for a
// freshly-claimed lease, epoch-guarded (the lease's current epoch). Idempotent:
// re-arming for the same (job, epoch) collides on the timer id and is ignored.
// Tests call this directly; the runtime arms them on every claim.
func (s *Store) ArmLeaseLivenessTimers(ctx context.Context, jobID string, epoch int, now time.Time, cfg LivenessConfig) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		// write the deadline columns (Rung-3 sub-state on the lease).
		phaseAt := now.Add(cfg.PhaseBudget)
		capAt := now.Add(cfg.AbsoluteCap)
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs SET phase_deadline_at = ?, lease_deadline = ?, last_heartbeat_at = ?,
			                rung2_last_verdict = 'abstain', rung2_window_head = '',
			                rung2_window_started_at = NULL, ci_running = 0,
			                updated_at = datetime('now')
			 WHERE id = ?`,
			phaseAt.Format(rfc3339), capAt.Format(rfc3339), now.Format(rfc3339), jobID); err != nil {
			return fmt.Errorf("set deadlines: %w", err)
		}
		if cfg.PhaseBudget > 0 {
			if err := armTimerTx(ctx, tx, fmt.Sprintf("%s-phase-e%d", jobID, epoch),
				jobID, TimerPhaseDeadline, phaseAt, epoch); err != nil {
				return err
			}
		}
		if cfg.AbsoluteCap > 0 {
			if err := armTimerTx(ctx, tx, fmt.Sprintf("%s-cap-e%d", jobID, epoch),
				jobID, TimerLeaseDeadline, capAt, epoch); err != nil {
				return err
			}
		}
		return nil
	})
}

// armTimerTx inserts a durable timer within tx, ignoring a duplicate id (idempotent
// re-arm). The expected_epoch is the fence: a later epoch makes the timer a no-op.
func armTimerTx(ctx context.Context, tx *sql.Tx, id, jobID, kind string, dueAt time.Time, epoch int) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO timers (id, job_id, kind, due_at, expected_epoch, fired)
		VALUES (?, ?, ?, ?, ?, 0)
		ON CONFLICT (id) DO NOTHING`,
		id, jobID, kind, dueAt.Format(rfc3339), epoch)
	return err
}

// RecordHeartbeatHints records the last Rung-0/Rung-1 worker-reported hints + the
// last-seen instant on a job's lease (fenced by epoch; a stale call is a no-op,
// mirroring the heartbeat fence). These feed the two-rung rule (they corroborate an
// un-gameable rung; they never kill alone). Called by the heartbeat handler.
func (s *Store) RecordHeartbeatHints(ctx context.Context, jobID string, epoch int,
	health liveness.AgentHealth, rung1 liveness.Rung1Class, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE jobs SET agent_health = ?, rung1_class = ?, last_heartbeat_at = ?, updated_at = datetime('now')
		 WHERE id = ? AND lease_epoch = ?`,
		string(health), string(rung1), now.Format(rfc3339), jobID, epoch)
	return err
}

// LivenessResult reports what an EvaluateLiveness pass did (for tests / publishing).
type LivenessResult struct {
	JobID     string
	Killed    bool      // a revoke fired (epoch bumped)
	ToState   job.State // the resulting state
	Reason    string    // revoke reason (absolute_cap / two_rung_stall / ...)
	Escalated bool      // routed to needs_human via the Rung-4 governor / attempts
	NewEpoch  int
}

// EvaluateLiveness runs the two-rung kill ladder for ONE active-lease job: it folds
// the current RungSet from persisted facts (Rung-3 clock comparison done HERE
// against Flowbee's clock; Rung-2 from the last sweep verdict; Rung-0/1 from the
// last heartbeat hints; the Rung-4 governor ceiling resolved from stall_revocations),
// runs the PURE engine.Decide(LivenessVerdict), and applies the returned revoke
// transaction (epoch++, compensation, state move) — all serialized. A non-kill is a
// no-op. breakerTripped is the fleet-wide circuit-breaker signal computed by the
// caller (Rung2Sweep). cfg carries the governor ceiling.
func (s *Store) EvaluateLiveness(ctx context.Context, jobID string, now time.Time,
	cfg LivenessConfig, breakerTripped bool) (LivenessResult, error) {
	var res LivenessResult
	res.JobID = jobID
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if !job.HasActiveLease(j.State) {
			return nil // nothing to evaluate (not an active lease)
		}
		// read the liveness sub-state on the lease.
		var health, rung1, rung2 string
		var phaseAt, capAt, hbAt sql.NullString
		if err := tx.QueryRowContext(ctx, `
			SELECT agent_health, rung1_class, rung2_last_verdict, phase_deadline_at, lease_deadline, last_heartbeat_at
			  FROM jobs WHERE id = ?`, jobID).
			Scan(&health, &rung1, &rung2, &phaseAt, &capAt, &hbAt); err != nil {
			return fmt.Errorf("read liveness substate: %w", err)
		}

		// Rung-3: pure wall-clock arithmetic on FLOWBEE's clock only (the sole clock).
		rs := liveness.RungSet{
			Health:                liveness.AgentHealth(health),
			Rung1:                 liveness.Rung1Class(rung1),
			Rung2:                 liveness.Rung2Verdict(rung2),
			CircuitBreakerTripped: breakerTripped,
		}
		if capAt.Valid {
			if t, perr := time.Parse(rfc3339, capAt.String); perr == nil && !now.Before(t) {
				rs.Rung3.AbsoluteCap = true
			}
		}
		// Heartbeat-staleness reap: a worker that hasn't checked in for longer than the
		// reap window is presumed DEAD (a clean crash reports no unhealthy hint). Measure
		// from the worker's last ACTIVITY = max(grant time, last heartbeat): the grant time
		// (lease_deadline - AbsoluteCap) covers the pre-first-beat setup phase and a freshly
		// re-claimed lease whose last_heartbeat_at is stale from the PRIOR worker — so a new
		// worker is never reaped before it has had the window to start beating.
		if cfg.HeartbeatReapAfter > 0 {
			lastSeen := time.Time{}
			if capAt.Valid && cfg.AbsoluteCap > 0 {
				if t, perr := time.Parse(rfc3339, capAt.String); perr == nil {
					lastSeen = t.Add(-cfg.AbsoluteCap) // grant time = deadline - cap
				}
			}
			if hbAt.Valid {
				if t, perr := time.Parse(rfc3339, hbAt.String); perr == nil && t.After(lastSeen) {
					lastSeen = t
				}
			}
			if !lastSeen.IsZero() && now.Sub(lastSeen) > cfg.HeartbeatReapAfter {
				rs.Rung3.HeartbeatStale = true
			}
		}
		if phaseAt.Valid {
			if t, perr := time.Parse(rfc3339, phaseAt.String); perr == nil && !now.Before(t) {
				rs.Rung3.SoftCrossed = true
			}
		}

		// Rung-4 governor (§10.7): resolve the anti-thrash ceiling + the §6.7 attempts
		// ceiling, passed into the engine so it picks the needs_human arm.
		governorReached := cfg.GovernorCeiling > 0 && j.StallRevocations+1 >= cfg.GovernorCeiling
		attemptsExhausted := j.Attempts+1 >= j.MaxAttempts

		dec := engine.Decide(
			engine.EngineState{Job: j, Now: now, Epoch: j.LeaseEpoch},
			engine.LivenessVerdict{
				Rungs:                  rs,
				GovernorCeilingReached: governorReached,
				AttemptsExhausted:      attemptsExhausted,
			})
		if len(dec.Transitions) == 0 {
			return nil // survived: no kill (§10.4 bias)
		}
		t := dec.Transitions[0]
		newEpoch := j.LeaseEpoch
		if t.BumpEpoch {
			newEpoch = j.LeaseEpoch + 1
		}
		if err := applyRevokeTx(ctx, tx, &j, seq, t, newEpoch, now); err != nil {
			return err
		}
		// M10 (§12.6.1): on an escalation to needs_human, stamp the canonical trigger
		// so the unified chokepoint view distinguishes the §6.7 conditions. The
		// absolute cap escalates for one of two reasons — max_attempts exhausted (the
		// attempts ceiling) or the Rung-4 governor (a genuine stall) — disambiguated
		// by the booleans the engine consumed. A two-rung stall kill is always stall.
		if t.To == job.StateNeedsHuman {
			reason := job.EscalationStall
			if attemptsExhausted && !governorReached {
				reason = job.EscalationAttempts
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE jobs SET escalation_reason = ? WHERE id = ?`, string(reason), j.ID); err != nil {
				return fmt.Errorf("stamp escalation reason: %w", err)
			}
		}
		res.Killed = true
		res.ToState = t.To
		res.Reason = t.RevokeReason
		res.Escalated = t.To == job.StateNeedsHuman
		res.NewEpoch = newEpoch
		return nil
	})
	if err != nil {
		return LivenessResult{}, err
	}
	return res, nil
}

// applyRevokeTx applies a liveness revoke transition: append the ledger event
// (carrying the bumped epoch + counter deltas), bump the epoch + clear the live
// lease + re-arm/escalate the job, close the lease audit row as revoked, and fire
// COMPENSATION (drop the dead epoch ref / cancel CI / draft-back — the M8 MVP
// records the compensation intent; the epoch bump is itself the zombie's fence so a
// reconnecting worker's next fenced call gets 409). All within the caller's tx, so a
// crash mid-revoke replays cleanly (§10.7).
func applyRevokeTx(ctx context.Context, tx *sql.Tx, j *job.Job, seq int,
	t engine.Transition, newEpoch int, now time.Time) error {
	nextSeq := seq + 1
	ev := ledger.Event{
		JobID: j.ID, JobSeq: nextSeq, Kind: t.Kind,
		FromState: t.From, ToState: t.To, LeaseEpoch: newEpoch,
		Actor: "system", CreatedAt: now,
		Payload: ledger.Payload{
			AttemptsDelta:         t.AttemptsDelta,
			StallRevocationsDelta: t.StallRevocationsDelta,
			RevokeReason:          t.RevokeReason,
		},
	}
	if t.To == job.StateReady {
		ev.Payload.Role = job.RoleEngWorker
	}
	if err := appendEvent(ctx, tx, ev); err != nil {
		return err
	}
	// the projection mutation: bump the epoch (the zombie's fence), clear the live
	// lease, advance state, increment the governor + attempts counters. On a
	// re-dispatch to `ready`/`review_pending` reset the deadline sub-state so the new
	// lease starts a fresh clock.
	roleClause := ""
	args := []any{
		newEpoch, t.AttemptsDelta, t.StallRevocationsDelta, string(t.To),
	}
	if t.To == job.StateReady {
		roleClause = ", role = 'eng_worker', required_capabilities = '[\"role:eng_worker\"]', enqueued_at = ?"
		args = append(args, now.Format(rfc3339))
	}
	args = append(args, j.ID)
	if _, err := tx.ExecContext(ctx, `
		UPDATE jobs
		   SET lease_epoch = ?,
		       attempts = attempts + ?,
		       stall_revocations = stall_revocations + ?,
		       state = ?,
		       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
		       lease_hb_due = NULL, lease_deadline = NULL, phase_deadline_at = NULL,
		       agent_health = '', rung1_class = '', rung2_last_verdict = 'abstain',
		       rung2_window_head = '', rung2_window_started_at = NULL, ci_running = 0`+roleClause+`,
		       updated_at = datetime('now')
		 WHERE id = ?`, args...); err != nil {
		return fmt.Errorf("apply revoke projection: %w", err)
	}
	if err := setJobSeq(ctx, tx, j.ID, nextSeq); err != nil {
		return err
	}
	// close the lease audit row with the right disposition.
	endReason := "revoked"
	switch t.RevokeReason {
	case "agent_exited":
		endReason = "expired"
	case "awaiting_input":
		endReason = "released"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE leases SET ended_at = datetime('now'), end_reason = ?
		 WHERE job_id = ? AND lease_epoch = ? AND ended_at IS NULL`,
		endReason, j.ID, j.LeaseEpoch); err != nil {
		return fmt.Errorf("close revoked lease: %w", err)
	}
	// cancel any pending deadline timers for the OLD epoch (the epoch guard already
	// makes them no-ops, but cancelling keeps the table tidy).
	if _, err := tx.ExecContext(ctx,
		`UPDATE timers SET fired = 1 WHERE job_id = ? AND expected_epoch = ? AND fired = 0`,
		j.ID, j.LeaseEpoch); err != nil {
		return fmt.Errorf("cancel old-epoch timers: %w", err)
	}
	return nil
}

// FireLeaseDeadline evaluates one due lease/phase-deadline timer (the River-replacing
// idempotent, epoch-guarded check, §3.5). It is a NO-OP when the timer is stale
// (lease_epoch advanced since arming -> the job was re-claimed / completed) or the
// job left its active-lease state. Otherwise it runs EvaluateLiveness, which decides
// (via the pure ladder) whether the deadline crossing kills: the ABSOLUTE cap
// revokes unilaterally; a SOFT deadline needs a second rung (so it may be a no-op
// kill-wise but still records the crossing as Rung-3 state). Returns whether a kill
// fired.
func (s *Store) FireLeaseDeadline(ctx context.Context, t DueTimer, now time.Time,
	cfg LivenessConfig, breakerTripped bool) (LivenessResult, error) {
	// epoch-guard + state-guard up front (cheap), then evaluate.
	var state string
	var epoch int
	err := s.DB.QueryRowContext(ctx,
		`SELECT state, lease_epoch FROM jobs WHERE id = ?`, t.JobID).Scan(&state, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		_ = s.markTimerFired(ctx, t.ID)
		return LivenessResult{JobID: t.JobID}, nil
	}
	if err != nil {
		return LivenessResult{}, err
	}
	if epoch != t.ExpectedEpoch || !job.HasActiveLease(job.State(state)) {
		_ = s.markTimerFired(ctx, t.ID) // stale: the job was re-claimed / progressed
		return LivenessResult{JobID: t.JobID}, nil
	}
	res, err := s.EvaluateLiveness(ctx, t.JobID, now, cfg, breakerTripped)
	if err != nil {
		return LivenessResult{}, err
	}
	// mark the timer fired (whether or not it killed): a soft-deadline that didn't
	// reach two-rung agreement is re-evaluated by the next Rung-2 sweep, not by
	// re-firing this one. The absolute cap, if not yet due, leaves its own timer.
	_ = s.markTimerFired(ctx, t.ID)
	return res, nil
}

// markTimerFired flips a timer to fired (standalone, outside a tx).
func (s *Store) markTimerFired(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE timers SET fired = 1 WHERE id = ?`, id)
	return err
}

// MarkTimerFired is the exported flip used by the poller when liveness is disabled
// (it cancels a stray deadline timer rather than acting on it).
func (s *Store) MarkTimerFired(ctx context.Context, id string) error {
	return s.markTimerFired(ctx, id)
}

// ActiveLeaseJobs returns the ids of every job currently holding an active lease
// (for the Rung-2 sweep + the liveness evaluation pass to iterate).
func (s *Store) ActiveLeaseJobs(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id FROM jobs
		 WHERE state IN ('leased','building','code_review','merging',
		                 'merge_handoff','spec_authoring','spec_review',
		                 'resolving_conflict')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ensure lease import is used (the sentinel surface the runtime maps to 409).
var _ = lease.ErrStaleEpoch
