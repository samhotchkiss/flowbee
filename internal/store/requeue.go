package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// RequeueJob re-arms an escalated / stranded job for a fresh attempt: it resets the
// attempt + bounce budget, clears the lease + verdict, bumps the lease epoch (fencing
// any zombie worker so its next call 409s), and routes the job back to `ready` as an
// eng_worker. This is the operator's "retry" for a job that escalated to needs_human
// from a now-fixed transient failure (e.g. a deployment bug) — without hand-editing
// the jobs table. Returns the resulting state.
func (s *Store) RequeueJob(ctx context.Context, jobID string, now time.Time) (job.State, error) {
	var final job.State
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		target := job.StateReady
		ev := ledger.Event{
			JobID: jobID, JobSeq: seq + 1, Kind: ledger.KindStateChanged,
			FromState: j.State, ToState: target, LeaseEpoch: j.LeaseEpoch + 1,
			Actor: "operator", CreatedAt: now,
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = ?, role = 'eng_worker', stage = 'build',
			       required_capabilities = ?,
			       head_sha = '', verdict = NULL,
			       attempts = 0, bounces = 0,
			       lease_epoch = lease_epoch + 1,
			       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
			       lease_hb_due = NULL, lease_deadline = NULL,
			       enqueued_at = ?, updated_at = datetime('now')
			 WHERE id = ?`,
			string(target), marshalStrings([]string{"role:eng_worker"}),
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
