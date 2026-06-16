package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// M11: Epoch-namespaced side-effects + compensation (DESIGN §3.5/§6.5, I-12).
//
// Fencing (lease_epoch) gives exactly-once ACKNOWLEDGEMENT into Flowbee; it does
// NOT reach git/CI/GitHub (T2). These helpers carry the idempotency fencing alone
// cannot: epoch-namespaced refs promoted only post-validation; (job, epoch)-scoped
// CI so a stale epoch's checks can never satisfy a live gate; and explicit
// compensation that drops the dead ref, cancels its CI, and drafts-back any PR.

// EpochCIState mirrors the GitHub statusCheckRollup conclusion, recorded per
// (job, epoch) so a zombie's green CI at a stale epoch never satisfies the live gate.
const (
	EpochCIPending   = "pending"
	EpochCISuccess   = "success"
	EpochCIFailure   = "failure"
	EpochCICancelled = "cancelled"
)

// RecordEpochCI records the CI conclusion for a specific (job, epoch) push (§6.5.2).
// CI is triggered by a worker's push to refs/flowbee/<job>/epoch-<n>; its result is
// keyed by that epoch. A revoked zombie that pushes to its STALE epoch and turns CI
// green writes a row for the DEAD epoch — never the live one — so LiveEpochCIGreen
// stays false for the live job. Idempotent upsert.
func (s *Store) RecordEpochCI(ctx context.Context, jobID string, epoch int, headSHA, state string, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO epoch_ci (job_id, epoch, head_sha, ci_state, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (job_id, epoch) DO UPDATE SET
		    head_sha = excluded.head_sha, ci_state = excluded.ci_state,
		    updated_at = excluded.updated_at`,
		jobID, epoch, headSHA, state, now.Format(rfc3339))
	return err
}

// LiveEpochCIGreen reports whether the job's LIVE build epoch has green CI (§6.5.2).
// The live epoch is build_epoch (the last PROMOTED epoch). A green row for any OTHER
// epoch — a stale zombie's push — does not count. This is the (job, epoch) gating
// that makes a reconnecting zombie's CI unable to satisfy the live gate.
func (s *Store) LiveEpochCIGreen(ctx context.Context, jobID string) (bool, error) {
	var buildEpoch int
	if err := s.DB.QueryRowContext(ctx, `SELECT build_epoch FROM jobs WHERE id = ?`, jobID).
		Scan(&buildEpoch); err != nil {
		return false, err
	}
	var state string
	err := s.DB.QueryRowContext(ctx,
		`SELECT ci_state FROM epoch_ci WHERE job_id = ? AND epoch = ?`, jobID, buildEpoch).
		Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state == EpochCISuccess, nil
}

// EpochCIStateFor returns the recorded CI state for a specific (job, epoch), or
// EpochCIPending if none recorded (for assertions).
func (s *Store) EpochCIStateFor(ctx context.Context, jobID string, epoch int) (string, error) {
	var state string
	err := s.DB.QueryRowContext(ctx,
		`SELECT ci_state FROM epoch_ci WHERE job_id = ? AND epoch = ?`, jobID, epoch).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return EpochCIPending, nil
	}
	return state, err
}

// epochGatedCITx reports the (job, epoch)-scoped CI verdict the code-review gate must
// honor (§6.5.2). It returns (inUse, liveGreen): inUse=true iff this job has ANY
// epoch_ci rows recorded (M11 epoch-CI is active for it); liveGreen=true iff the LIVE
// build epoch's row is success. When inUse, the gate ANDs the reconciled CIGreen with
// liveGreen — so a zombie's green CI recorded against a STALE epoch (build_epoch !=
// that epoch) can never satisfy the live gate. When not inUse (pre-M11 jobs / tests
// without epoch CI), the gate is unchanged (the reconciled CIGreen alone decides).
func epochGatedCITx(ctx context.Context, tx *sql.Tx, jobID string) (inUse, liveGreen bool, err error) {
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM epoch_ci WHERE job_id = ?`, jobID).Scan(&n); err != nil {
		return false, false, err
	}
	if n == 0 {
		return false, false, nil
	}
	var buildEpoch int
	if err = tx.QueryRowContext(ctx, `SELECT build_epoch FROM jobs WHERE id = ?`, jobID).Scan(&buildEpoch); err != nil {
		return false, false, err
	}
	var state string
	e := tx.QueryRowContext(ctx,
		`SELECT ci_state FROM epoch_ci WHERE job_id = ? AND epoch = ?`, jobID, buildEpoch).Scan(&state)
	if errors.Is(e, sql.ErrNoRows) {
		return true, false, nil
	}
	if e != nil {
		return true, false, e
	}
	return true, state == EpochCISuccess, nil
}

// PromoteResult validates that a result's claimed epoch is the job's LIVE lease epoch
// and, only then, fast-forwards the epoch-namespaced ref onto the real branch and
// records build_epoch (§6.5.1: "promote only post-validation; a stale epoch's ref is
// orphaned, never promoted"). A stale epoch (the lease was revoked + re-dispatched
// since) returns promoted=false and touches nothing — its ref is left orphaned for
// compensation to drop. The mirror is optional (nil in tests with no git fixture):
// the epoch validation + build_epoch bump still run so the (job, epoch) CI gate works
// without a live mirror.
func (s *Store) PromoteResult(ctx context.Context, m *gitops.Mirror, jobID string, epoch int, branch string, now time.Time) (bool, error) {
	promoted := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		// epoch validation (the fence): only the LIVE lease epoch may be promoted. A
		// stale epoch (revoked + re-dispatched) is orphaned, never fast-forwarded.
		if epoch != j.LeaseEpoch {
			return nil // stale epoch -> not promoted (orphaned)
		}
		// fast-forward the real branch from the epoch ref (Flowbee's sole-promoter
		// step, I-7/I-12) — only when a mirror is configured.
		epochRef := gitops.EpochRef(jobID, epoch)
		if m != nil && branch != "" {
			if _, ok := m.RefSHA(epochRef); ok {
				if err := m.PromoteEpochRef(epochRef, branch); err != nil {
					return fmt.Errorf("promote epoch ref: %w", err)
				}
			}
		}
		// record build_epoch (the live build epoch the (job, epoch) CI gate reads) and
		// mark the epoch_ci row promoted.
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET build_epoch = ?, updated_at = datetime('now') WHERE id = ?`,
			epoch, jobID); err != nil {
			return fmt.Errorf("set build_epoch: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO epoch_ci (job_id, epoch, ci_state, promoted, updated_at)
			VALUES (?, ?, 'pending', 1, ?)
			ON CONFLICT (job_id, epoch) DO UPDATE SET promoted = 1, updated_at = excluded.updated_at`,
			jobID, epoch, now.Format(rfc3339)); err != nil {
			return fmt.Errorf("mark epoch promoted: %w", err)
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindEpochPromoted,
			FromState: j.State, ToState: j.State, LeaseEpoch: j.LeaseEpoch,
			Actor: "system", CreatedAt: now,
			Payload: ledger.Payload{BuildEpoch: epoch},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if err := setJobSeq(ctx, tx, jobID, nextSeq); err != nil {
			return err
		}
		promoted = true
		return nil
	})
	return promoted, err
}

// CompensateParams describes a compensate(job, dead_epoch) call (§6.5.4, I-12).
type CompensateParams struct {
	JobID     string
	DeadEpoch int
	Reason    string
	// Branch/Mirror let compensation drop the dead epoch's ref (orphan the zombie's
	// work). Mirror is optional (nil when no git fixture); the ref-drop is then a
	// recorded intent only. EnqueueDraftBack=true enqueues a project-OUT draft-back of
	// any PR opened for the dead attempt (never leave a ready zombie PR).
	Mirror           *gitops.Mirror
	EnqueueDraftBack bool
	Now              time.Time
}

// CompensateResult reports which compensation actions ran (for tests/audit).
type CompensateResult struct {
	RefDropped  bool
	CICancelled bool
	PRDrafted   bool
	AlreadyDone bool // a compensation for this (job, dead_epoch) already ran (idempotent)
}

// Compensate runs the explicit compensation for a revoked/expired/superseded epoch
// (§6.5.4): drop refs/flowbee/<job>/epoch-<dead_epoch>, cancel that epoch's CI, and
// (if a draft PR was opened for this attempt) draft-back the PR. The epoch was ALREADY
// bumped by the revoke/supersede transaction that triggered this — so the reconnecting
// worker is already fenced 409; compensation cleans up the side-effects fencing cannot
// reach. Keyed (job, dead_epoch) so re-running is a no-op (idempotent). The actual CI
// cancel of the GitHub-side run is enqueued via project-OUT is unnecessary here — the
// (job, epoch) CI row is marked cancelled locally so the live gate ignores it; a real
// GitHub cancel rides project-OUT in production but is a best-effort no-op in the fake.
func (s *Store) Compensate(ctx context.Context, p CompensateParams) (CompensateResult, error) {
	var res CompensateResult
	// drop the dead epoch's ref OUTSIDE the tx (filesystem I/O), best-effort.
	deadRef := gitops.EpochRef(p.JobID, p.DeadEpoch)
	if p.Mirror != nil {
		if err := p.Mirror.DropRef(deadRef); err != nil {
			return res, fmt.Errorf("compensate drop ref: %w", err)
		}
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// idempotency: a compensation already recorded for this (job, dead_epoch) is a
		// no-op (a crash mid-compensate replays cleanly, §6.5).
		var existing int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM compensations WHERE job_id = ? AND dead_epoch = ?`,
			p.JobID, p.DeadEpoch).Scan(&existing)
		if err == nil {
			res.AlreadyDone = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("compensation lookup: %w", err)
		}

		j, seq, err := loadJobTx(ctx, tx, p.JobID)
		if err != nil {
			return err
		}

		// cancel the (job, dead_epoch) CI locally: mark the dead epoch's CI cancelled so
		// the live gate (LiveEpochCIGreen on build_epoch) can never read it as green.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO epoch_ci (job_id, epoch, ci_state, updated_at)
			VALUES (?, ?, 'cancelled', ?)
			ON CONFLICT (job_id, epoch) DO UPDATE SET ci_state = 'cancelled', updated_at = excluded.updated_at`,
			p.JobID, p.DeadEpoch, p.Now.Format(rfc3339)); err != nil {
			return fmt.Errorf("cancel dead-epoch CI: %w", err)
		}
		res.CICancelled = true
		res.RefDropped = p.Mirror != nil

		// draft-back any PR opened for this attempt (never leave a ready zombie PR).
		var prNumber sql.NullInt64
		_ = tx.QueryRowContext(ctx, `SELECT pr_number FROM jobs WHERE id = ?`, p.JobID).Scan(&prNumber)
		if p.EnqueueDraftBack && prNumber.Valid && prNumber.Int64 > 0 {
			if err := enqueueOutboxTx(ctx, tx, OutboxRow{
				JobID:   p.JobID,
				Action:  ActionDraftPR,
				HeadSHA: fmt.Sprintf("epoch-%d", p.DeadEpoch), // distinct key per dead epoch
				Payload: outboxPayload(map[string]any{"pr_number": int(prNumber.Int64)}),
			}); err != nil {
				return fmt.Errorf("enqueue draft-back: %w", err)
			}
			res.PRDrafted = true
		}

		// record the compensation (idempotency backbone + audit).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO compensations (job_id, dead_epoch, ref_dropped, ci_cancelled, pr_drafted, reason, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.JobID, p.DeadEpoch, b2i(res.RefDropped), b2i(res.CICancelled), b2i(res.PRDrafted),
			p.Reason, p.Now.Format(rfc3339)); err != nil {
			return fmt.Errorf("record compensation: %w", err)
		}

		// append the compensated audit event (replay/audit completeness).
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: p.JobID, JobSeq: nextSeq, Kind: ledger.KindCompensated,
			FromState: j.State, ToState: j.State, LeaseEpoch: j.LeaseEpoch,
			Actor: "system", CreatedAt: p.Now,
			Payload: ledger.Payload{DeadEpoch: p.DeadEpoch, RevokeReason: p.Reason},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		return setJobSeq(ctx, tx, p.JobID, nextSeq)
	})
	return res, err
}

// RecordUnattendedMerge stamps the reconciled merge-commit provenance on a job that
// merged unattended via the queue (§14 Branch B). It is called AFTER reconcile-IN has
// observed the merged terminal fact (so the job is already `done`); this only records
// the provenance + the audit event proving the merge happened with no human in the
// loop, attributed to the reviewer's minted self_merge verdict (I-9 provenance).
func (s *Store) RecordUnattendedMerge(ctx context.Context, jobID, mergeCommit string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET merge_provenance = ?, updated_at = datetime('now') WHERE id = ?`,
			mergeCommit, jobID); err != nil {
			return fmt.Errorf("stamp merge provenance: %w", err)
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindUnattendedMerged,
			FromState: j.State, ToState: j.State, LeaseEpoch: j.LeaseEpoch,
			Actor: "system", CreatedAt: now,
			Payload: ledger.Payload{MergeProvenance: mergeCommit},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		return setJobSeq(ctx, tx, jobID, nextSeq)
	})
}

// CompensationFor returns the recorded compensation for a (job, dead_epoch), if any.
func (s *Store) CompensationFor(ctx context.Context, jobID string, deadEpoch int) (CompensateResult, bool, error) {
	var refDropped, ciCancelled, prDrafted int
	err := s.DB.QueryRowContext(ctx,
		`SELECT ref_dropped, ci_cancelled, pr_drafted FROM compensations WHERE job_id = ? AND dead_epoch = ?`,
		jobID, deadEpoch).Scan(&refDropped, &ciCancelled, &prDrafted)
	if errors.Is(err, sql.ErrNoRows) {
		return CompensateResult{}, false, nil
	}
	if err != nil {
		return CompensateResult{}, false, err
	}
	return CompensateResult{
		RefDropped: refDropped == 1, CICancelled: ciCancelled == 1, PRDrafted: prDrafted == 1,
	}, true, nil
}
