package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// ErrJobNotFound is returned when an operation targets a job id that doesn't exist, so
// the API can answer 404 (not a 500). A truncated/mistyped id is operator error, not a
// server fault — surfacing it as 500 made the documented recovery path look broken.
var ErrJobNotFound = errors.New("job not found")

// RequeueJob re-arms an escalated / stranded job for a fresh attempt: it resets the
// attempt + bounce budget, clears the lease + verdict, bumps the lease epoch (fencing
// any zombie worker so its next call 409s), and routes the job back to `ready` as an
// eng_worker. This is the operator's "retry" for a job that escalated to needs_human
// from a now-fixed transient failure (e.g. a deployment bug) — without hand-editing
// the jobs table. Returns the resulting state.
// ErrJobActivelyLeased is returned when a requeue targets a job that currently holds an
// active lease (a worker is building/reviewing/merging it right now). Re-arming it bumps
// the epoch, which FENCES the live worker — silently discarding its in-flight work. The
// requeue is for STRANDED jobs (needs_human/failed), so this is rejected unless forced.
var ErrJobActivelyLeased = errors.New("job is actively leased; requeue would discard the live worker's in-flight work (use force to override)")

// RequeueJob re-arms an escalated / stranded job for a fresh attempt: it resets the
// attempt + bounce budget, clears the lease + verdict, bumps the lease epoch (fencing
// any zombie worker so its next call 409s), and routes the job back to `ready` as an
// eng_worker. This is the operator's "retry" for a job that escalated to needs_human
// from a now-fixed transient failure (e.g. a deployment bug) — without hand-editing
// the jobs table. Returns the resulting state. When force is false (the default), a job
// that holds an ACTIVE lease is rejected (ErrJobActivelyLeased) so a mistyped id or a
// just-picked-up job doesn't have a live worker's build silently fenced + discarded.
func (s *Store) RequeueJob(ctx context.Context, jobID string, force bool, now time.Time) (job.State, error) {
	var final job.State
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrJobNotFound
		}
		if err != nil {
			return err
		}
		if !force && job.HasActiveLease(j.State) {
			return ErrJobActivelyLeased
		}
		// Re-arm to the job's OWN entry stage: a spec job restarts at spec_authoring (it
		// has no build to rebuild), a build job at `ready`. Routing a spec escalation to a
		// build (the old unconditional behavior) would re-arm it as a buildable job with no
		// spec, which just fails again.
		target, role, stage, cap := job.StateReady, "eng_worker", "build", "role:eng_worker"
		if j.Kind == job.KindSpec {
			target, role, stage, cap = job.StateSpecAuthoring, string(job.RoleSpecAuthor), "spec", "role:spec_author"
		}
		ev := ledger.Event{
			JobID: jobID, JobSeq: seq + 1, Kind: ledger.KindStateChanged,
			FromState: j.State, ToState: target, LeaseEpoch: j.LeaseEpoch + 1,
			Actor: "operator", CreatedAt: now,
			// the requeue zeroes the attempts/bounces budget; carry that on the event so a
			// re-fold reproduces it (over_budget + escalation_reason clear via the state rule).
			Payload: ledger.Payload{ResetCounters: true},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		// Clear build-attempt artifacts too. A requeued job is a fresh build candidate;
		// carrying a previous attempt's diff/blast-radius makes reservation filtering treat
		// the stale write-set as current and can withhold the entire ready queue.
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = ?, role = ?, stage = ?,
			       required_capabilities = ?,
			       head_sha = '', verdict = NULL,
			       patch_diff = '', declared_blast_radius = '',
			       reservation_paths = '', reservation_wide = 0,
			       attempts = 0, bounces = 0, stall_revocations = 0,
			       over_budget = 0, escalation_reason = '',
			       lease_epoch = lease_epoch + 1,
			       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
			       lease_hb_due = NULL, lease_deadline = NULL,
			       enqueued_at = ?, updated_at = datetime('now')
			 WHERE id = ?`,
			string(target), role, stage, marshalStrings([]string{cap}),
			now.Format(rfc3339), jobID); err != nil {
			return fmt.Errorf("requeue: %w", err)
		}
		// close any open lease audit row.
		if _, err := tx.ExecContext(ctx, `
			UPDATE leases SET ended_at = datetime('now'), end_reason = 'requeued'
			 WHERE job_id = ? AND ended_at IS NULL`, jobID); err != nil {
			return err
		}
		final = target
		return setJobSeq(ctx, tx, jobID, seq+1)
	})
	return final, err
}

// CancelJob terminally CANCELS a job the operator has decided NOT to pursue — the
// complement to RequeueJob (give up vs. retry). It transitions the job to `cancelled`
// (a terminal state), bumps the lease epoch (fencing any worker), and clears the lease,
// event-sourced. Use it to clear a rejected/dead-end job from the needs_human triage view
// without hand-editing the table. Idempotent: a no-op on an already-terminal job. Like
// requeue, a job holding an ACTIVE lease is rejected unless force (cancelling fences the
// live worker, discarding its in-flight work). Returns the resulting state.
func (s *Store) CancelJob(ctx context.Context, jobID string, force bool, now time.Time) (job.State, error) {
	var final job.State
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrJobNotFound
		}
		if err != nil {
			return err
		}
		if j.State == job.StateDone || j.State == job.StateCancelled {
			final = j.State // already terminal: idempotent no-op
			return nil
		}
		if !force && job.HasActiveLease(j.State) {
			return ErrJobActivelyLeased
		}
		ev := ledger.Event{
			JobID: jobID, JobSeq: seq + 1, Kind: ledger.KindStateChanged,
			FromState: j.State, ToState: job.StateCancelled, LeaseEpoch: j.LeaseEpoch + 1,
			Actor: "operator", CreatedAt: now,
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = 'cancelled',
			       lease_epoch = lease_epoch + 1,
			       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
			       lease_hb_due = NULL, lease_deadline = NULL, updated_at = datetime('now')
			 WHERE id = ?`, jobID); err != nil {
			return fmt.Errorf("cancel: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE leases SET ended_at = datetime('now'), end_reason = 'cancelled'
			 WHERE job_id = ? AND ended_at IS NULL`, jobID); err != nil {
			return err
		}
		final = job.StateCancelled
		return setJobSeq(ctx, tx, jobID, seq+1)
	})
	return final, err
}
