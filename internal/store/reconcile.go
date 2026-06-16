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

// ReconciledPR is the Domain-B fact-set for one PR, as reconcile-IN observed it
// from a sweep or a targeted refetch (§8.1). It carries ONLY GitHub-owned facts
// (§3.4); reconcile-IN may write nothing else. The store-side ingest below is the
// structural guarantee of that asymmetry — it only ever touches Domain-B columns
// and the reconcile-driven state transitions (superseded re-arm / terminal freeze)
// the §3.4 rules mandate. It never writes a stage role, lens, verdict, or counter.
type ReconciledPR struct {
	Number      int
	UpdatedAt   time.Time
	IsDraft     bool
	Merged      bool
	HeadSHA     string
	BaseSHA     string
	MergeCommit string
	CIGreen     bool
}

// ReconcileOutcome reports what an ingest did, for the runtime to publish / assert.
type ReconcileOutcome struct {
	JobID      string
	Applied    bool // facts written (passed the monotonic guard)
	Superseded bool // a SHA move re-armed the job (I-5, §6.2.4)
	Frozen     bool // terminal-SHA guard fired: merged job, no re-dispatch (I-3)
	Done       bool // a non-terminal job whose PR merged transitioned to done
}

// ApplyReconciledPR ingests one PR's Domain-B facts for the job bound to that PR
// number, applying the I-3 guards and the §3.4 reconcile-driven transitions. It is
// the ONLY writer of Domain-B fact-fields (I-1). It NEVER writes a Domain-A field
// (stage/role/lens/verdict/counters): a reconcile that "disagrees" about a
// Flowbee-owned fact is ignored — Flowbee is right and there is nothing to
// reconcile (§8.1.2).
//
// Guards, in order (I-3, §8.1.5):
//  1. SHA-monotonic: an ingest whose updatedAt is older than the recorded
//     high-water-mark is ignored (late/out-of-order delivery cannot rewind state).
//  2. Terminal-SHA: a job whose recorded merge commit is set is FROZEN — no event
//     re-dispatches it (closes the double-merge failure at ingestion).
//
// Then the §3.4 reconcile-driven transitions:
//   - merged PR + non-terminal job -> done (the terminal Domain-B fact).
//   - a head/base SHA MOVE on an open PR whose job holds a SHA-bound verdict ->
//     superseded + re-arm (I-5): invalidate the verdict, route to ready with the
//     new base, revoke any active lease (epoch bump), re-run review + CI.
//
// jobID is resolved by the caller from pr_number; an unknown PR is a no-op.
func (s *Store) ApplyReconciledPR(ctx context.Context, jobID string, pr ReconciledPR, now time.Time) (ReconcileOutcome, error) {
	out := ReconcileOutcome{JobID: jobID}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}

		// read the prior reconciled facts (the monotonic + terminal high-water-marks).
		var priorUpdated, priorMergeCommit string
		err = tx.QueryRowContext(ctx,
			`SELECT head_updated_at, merge_commit FROM domain_b_facts WHERE job_id = ?`, jobID).
			Scan(&priorUpdated, &priorMergeCommit)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read prior facts: %w", err)
		}

		// TERMINAL-SHA guard (I-3): a job with a recorded merge commit is frozen.
		if priorMergeCommit != "" {
			out.Frozen = true
			return nil
		}

		// SHA-monotonic guard (I-3): ignore an ingest older than the recorded mark.
		if priorUpdated != "" && !pr.UpdatedAt.IsZero() {
			if prev, perr := time.Parse(rfc3339, priorUpdated); perr == nil {
				if pr.UpdatedAt.Before(prev) {
					return nil // stale: cannot rewind state
				}
			}
		}

		// detect a SHA move BEFORE we overwrite the stored facts. A move is a change
		// of head OR base from the previously-reconciled values, on an open PR.
		var prevHead, prevBase string
		_ = tx.QueryRowContext(ctx,
			`SELECT head_sha, base_sha FROM domain_b_facts WHERE job_id = ?`, jobID).
			Scan(&prevHead, &prevBase)
		// A move is a CHANGE from a previously-reconciled value — not the first time
		// we LEARN one. Both terms therefore require the prior value to be non-empty:
		// an early sweep can report a head but an empty base oid (or vice versa), and
		// later filling it in must NOT read as a base move (which would spuriously
		// supersede a perfectly good verdict and re-arm the build).
		shaMoved := !pr.Merged &&
			(prevHead != "" && prevHead != pr.HeadSHA ||
				prevBase != "" && pr.BaseSHA != "" && prevBase != pr.BaseSHA)

		// write the Domain-B facts (the ONLY columns reconcile-IN may touch).
		if err := upsertDomainBFactsTx(ctx, tx, jobID, pr); err != nil {
			return err
		}
		out.Applied = true

		// reconcile-driven transitions (§3.4). These move STATE only as a consequence
		// of a GitHub-owned fact changing — never a stage/role/verdict edit.
		switch {
		case pr.Merged && j.State != job.StateDone:
			// the terminal Domain-B fact: the job is done. No counter or verdict edit.
			if err := reconcileTransitionTx(ctx, tx, &j, seq, job.StateDone,
				ledger.KindJobCompleted, now,
				ledger.Payload{MergeProvenance: pr.MergeCommit}); err != nil {
				return err
			}
			// build-list §F: on merge, enqueue the dedicated post-merge history
			// write (docs/history/<id>.md + the regenerated TOC) in THIS tx, so the
			// issue-archive projection lands atomically with the done transition and
			// is never entangled with the feature PR. Flowbee is the sole writer.
			if err := enqueueHistoryWriteTx(ctx, tx, jobID); err != nil {
				return err
			}
			out.Done = true
		case shaMoved && supersedable(j.State):
			// I-5 / §6.2.4: a head/base move supersedes the SHA-bound verdict and
			// re-arms review + CI. Invalidate the verdict, revoke any active lease
			// (epoch bump -> a still-running worker is fenced 409 on its next call),
			// route to ready with the new base.
			if err := supersedeTx(ctx, tx, &j, seq, pr, now); err != nil {
				return err
			}
			out.Superseded = true
		}
		return nil
	})
	if err != nil {
		return ReconcileOutcome{}, err
	}
	return out, nil
}

// supersedable reports whether a state can be superseded by a SHA move. Any active
// or mergeable build state (§6.2.4). Terminal/needs-human/superseded are not.
func supersedable(s job.State) bool {
	switch s {
	case job.StateLeased, job.StateBuilding, job.StateReviewPending,
		job.StateCodeReview, job.StateMergeable, job.StateMerging, job.StateMergeHandoff:
		return true
	default:
		return false
	}
}

// upsertDomainBFactsTx writes ONLY Domain-B columns. The monotonic high-water-mark
// (head_updated_at) and terminal fact (merge_commit) are written here so the next
// ingest's guards see them.
func upsertDomainBFactsTx(ctx context.Context, tx *sql.Tx, jobID string, pr ReconciledPR) error {
	updated := ""
	if !pr.UpdatedAt.IsZero() {
		updated = pr.UpdatedAt.Format(rfc3339)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO domain_b_facts
		    (job_id, pr_exists, pr_number, head_sha, base_sha, ci_green, merged,
		     head_updated_at, merge_commit, is_draft, updated_at)
		VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT (job_id) DO UPDATE SET
		    pr_exists = 1, pr_number = excluded.pr_number,
		    head_sha = excluded.head_sha, base_sha = excluded.base_sha,
		    ci_green = excluded.ci_green, merged = excluded.merged,
		    head_updated_at = excluded.head_updated_at,
		    merge_commit = excluded.merge_commit,
		    is_draft = excluded.is_draft, updated_at = datetime('now')`,
		jobID, pr.Number, pr.HeadSHA, pr.BaseSHA, b2i(pr.CIGreen), b2i(pr.Merged),
		updated, pr.MergeCommit, b2i(pr.IsDraft))
	return err
}

// reconcileTransitionTx appends a state-changed ledger event and applies the
// projection, all in tx. Used for the merged->done terminal transition. The
// optional payload carries resolved facts the event should record (e.g. the
// reconciled merge-commit on a merged->done, so the §F archive can fold it).
func reconcileTransitionTx(ctx context.Context, tx *sql.Tx, j *job.Job, seq int,
	to job.State, kind ledger.EventKind, now time.Time, payload ...ledger.Payload) error {
	nextSeq := seq + 1
	ev := ledger.Event{
		JobID: j.ID, JobSeq: nextSeq, Kind: kind,
		FromState: j.State, ToState: to, LeaseEpoch: j.LeaseEpoch,
		Actor: "reconcile", CreatedAt: now,
	}
	if len(payload) > 0 {
		ev.Payload = payload[0]
	}
	if err := appendEvent(ctx, tx, ev); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, updated_at = datetime('now') WHERE id = ?`,
		string(to), j.ID); err != nil {
		return fmt.Errorf("apply reconcile transition: %w", err)
	}
	return setJobSeq(ctx, tx, j.ID, nextSeq)
}

// supersedeTx realizes I-5 / §6.2.4: invalidate the SHA-bound verdict, revoke any
// active lease (epoch bump => a still-running worker is fenced), re-arm to ready
// with the new base. It writes the new base_sha (a Domain-B fact) and clears the
// verdict (whose binding is now stale) — clearing a now-invalid verdict is part of
// the supersession the SHA owner triggers, not an edit of a live Domain-A decision.
func supersedeTx(ctx context.Context, tx *sql.Tx, j *job.Job, seq int, pr ReconciledPR, now time.Time) error {
	nextSeq := seq + 1
	// the supersede event records the move for replay/audit.
	ev := ledger.Event{
		JobID: j.ID, JobSeq: nextSeq, Kind: ledger.KindSuperseded,
		FromState: j.State, ToState: job.StateReady, LeaseEpoch: j.LeaseEpoch + 1,
		Actor: "reconcile", CreatedAt: now,
		Payload: ledger.Payload{BaseSHA: pr.BaseSHA},
	}
	if err := appendEvent(ctx, tx, ev); err != nil {
		return err
	}
	// revoke any active lease by bumping the epoch (compensation's fence, §6.5): a
	// still-running worker's next fenced call carries the old epoch -> 409. Re-arm
	// to ready as an eng_worker against the new base; invalidate the verdict.
	if _, err := tx.ExecContext(ctx, `
		UPDATE jobs
		   SET state = 'ready', role = 'eng_worker', stage = 'build',
		       required_capabilities = ?,
		       base_sha = ?, head_sha = '',
		       verdict = NULL,
		       lease_epoch = lease_epoch + 1,
		       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
		       lease_hb_due = NULL, lease_deadline = NULL,
		       enqueued_at = ?, updated_at = datetime('now')
		 WHERE id = ?`,
		marshalStrings([]string{"role:eng_worker"}), pr.BaseSHA,
		now.Format(rfc3339), j.ID); err != nil {
		return fmt.Errorf("apply supersede: %w", err)
	}
	// close any open lease audit row as superseded.
	if _, err := tx.ExecContext(ctx, `
		UPDATE leases SET ended_at = datetime('now'), end_reason = 'superseded'
		 WHERE job_id = ? AND ended_at IS NULL`, j.ID); err != nil {
		return fmt.Errorf("close superseded lease: %w", err)
	}
	return setJobSeq(ctx, tx, j.ID, nextSeq)
}

// JobIDForPR resolves the job bound to a GitHub PR number. ok=false if no job is
// bound to that PR (an un-adopted PR; reconcile-IN no-ops on it). Used to map a
// swept/refetched PR fact back to a Domain-A job.
func (s *Store) JobIDForPR(ctx context.Context, prNumber int) (string, bool, error) {
	var id string
	err := s.DB.QueryRowContext(ctx,
		`SELECT id FROM jobs WHERE pr_number = ? LIMIT 1`, prNumber).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// BindPRNumber stamps the GitHub PR number onto a job (the §7.3 PR-open trigger
// stamps it in M7; M6 tests bind it directly to associate a swept PR with a job).
// pr_number is GitHub-owned (Domain B) and only ever written by the PR-open path /
// reconcile binding — never by a worker.
func (s *Store) BindPRNumber(ctx context.Context, jobID string, prNumber int) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE jobs SET pr_number = ?, updated_at = datetime('now') WHERE id = ?`,
		prNumber, jobID)
	return err
}

// SetReconciledFacts is a test/seed helper that writes a job's initial reconciled
// facts (the monotonic baseline) WITHOUT running the guards or transitions. Used to
// establish a prior reconcile state before driving a sweep that moves it.
func (s *Store) SetReconciledFacts(ctx context.Context, jobID string, pr ReconciledPR) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return upsertDomainBFactsTx(ctx, tx, jobID, pr)
	})
}
