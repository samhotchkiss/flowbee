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
	// CIFailed is true only on a DEFINITIVE CI failure at the head (FAILURE/ERROR,
	// not PENDING): the build is broken. A review_pending job then bounces back to
	// build (rebuild), escalating to needs_human at max_bounces. Transient (not
	// stored) — recomputed from the rollup each sweep.
	CIFailed bool
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
		//
		// flowbeePlaced: the PR sits EXACTLY where Flowbee last placed the branch —
		// j.head_sha / j.base_sha are the atomic record of the build result, a
		// rebase-before-review, or a conflict resolution. That advance is Flowbee's OWN
		// (it performed the git write), NOT an external move, so it must NOT supersede:
		// otherwise our own rebase/resolve push reads as a SHA move and kicks the review
		// back to build, churning (the live resolve→supersede→rebuild loop). This is
		// race-free where a domain_b_facts write is not — the JOB row is set atomically
		// with the state transition, independent of when the git push lands on GitHub.
		// An EXTERNAL push (pr.head != j.head_sha) or main advancing PAST where we rebased
		// (pr.base != j.base_sha) still differs from the job record -> a real move.
		flowbeePlaced := j.HeadSHA != "" && pr.HeadSHA == j.HeadSHA &&
			(j.BaseSHA == "" || pr.BaseSHA == "" || pr.BaseSHA == j.BaseSHA)
		shaMoved := !pr.Merged && !flowbeePlaced &&
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
			// base_sha refresh after merge: advance every still-`ready` build in this repo
			// to the new main (the merge commit) so it builds on CURRENT code, not the
			// stale base it was adopted at. Jobs with a PR re-base via supersede /
			// rebase-before-review; this closes the gap for not-yet-built ready jobs.
			if pr.MergeCommit != "" {
				if err := refreshReadyBaseTx(ctx, tx, j.Repo, jobID, pr.MergeCommit, now); err != nil {
					return err
				}
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
		case pr.CIFailed && j.State == job.StateReviewPending:
			// the build's CI is DEFINITIVELY red while it awaits review: the change is
			// broken, so bounce it back to build (rebuild), escalating to needs_human
			// at max_bounces. Gated to review_pending so a single failure bounces once
			// (the rebuild moves the head; the next sweep sees fresh/pending CI).
			if err := ciFailBounceTx(ctx, tx, &j, seq, now); err != nil {
				return err
			}
			out.Applied = true
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
		job.StateCodeReview, job.StateMergeable:
		return true
	default:
		// merge_handoff is DELIBERATELY excluded: it is the "a human merges" state
		// (self-merge denied — e.g. a change to Flowbee's own source hits the
		// flowbee_source denylist). The reviewer's empty findings-commit moves the
		// branch head AFTER the verdict bound to the reviewed head, so leaving
		// merge_handoff supersedable re-armed it on every sweep → an infinite
		// handoff→supersede→rebuild→re-review loop the human could never merge into.
		// A handed-off job must SETTLE; if a human pushes a real fix to the branch and
		// wants re-review, they `flowbee requeue` it explicitly. merging is excluded
		// too — a merge in flight must not be yanked back to build mid-dispatch.
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

// ciFailBounceTx re-arms a review_pending job whose CI is definitively red back to
// build (a rebuild), or escalates to needs_human at max_bounces. It mirrors the
// gate's own bounce events (KindReviewBounced / KindBounceExhausted) EXACTLY so the
// jobs projection stays equal to a re-fold of the ledger (determinism).
func ciFailBounceTx(ctx context.Context, tx *sql.Tx, j *job.Job, seq int, now time.Time) error {
	nextSeq := seq + 1
	if j.Bounces+1 > j.MaxBounces {
		// the rebuild keeps failing CI: escalate to a human (KindBounceExhausted fold:
		// state = ToState, bounces += delta, lease cleared).
		ev := ledger.Event{
			JobID: j.ID, JobSeq: nextSeq, Kind: ledger.KindBounceExhausted,
			FromState: j.State, ToState: job.StateNeedsHuman, LeaseEpoch: j.LeaseEpoch,
			Actor: "reconcile", CreatedAt: now, Payload: ledger.Payload{BouncesDelta: 1},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs SET state='needs_human', bounces=bounces+1,
			       lease_id=NULL, bound_identity=NULL, bound_model_family=NULL,
			       updated_at=datetime('now') WHERE id=?`, j.ID); err != nil {
			return fmt.Errorf("apply ci-fail escalate: %w", err)
		}
		return setJobSeq(ctx, tx, j.ID, nextSeq)
	}
	// bounce to ready as an eng_worker for a fresh build (KindReviewBounced fold:
	// state = ToState, bounces += delta, role = eng_worker, enqueued_at = now,
	// lease cleared).
	ev := ledger.Event{
		JobID: j.ID, JobSeq: nextSeq, Kind: ledger.KindReviewBounced,
		FromState: j.State, ToState: job.StateReady, LeaseEpoch: j.LeaseEpoch,
		Actor: "reconcile", CreatedAt: now, Payload: ledger.Payload{BouncesDelta: 1},
	}
	if err := appendEvent(ctx, tx, ev); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state='ready', role='eng_worker', required_capabilities=?,
		       bounces=bounces+1,
		       enqueued_at=?, lease_id=NULL, bound_identity=NULL, bound_model_family=NULL,
		       updated_at=datetime('now') WHERE id=?`,
		marshalStrings([]string{"role:eng_worker"}), now.Format(rfc3339), j.ID); err != nil {
		return fmt.Errorf("apply ci-fail bounce: %w", err)
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

// refreshReadyBaseTx advances every still-`ready` build in repo (except the just-merged
// job) to newBase — the new main HEAD after a sibling merge — so a job adopted at an
// older base now builds on CURRENT code. Each refresh emits KindBaseRefreshed so the
// projection equals a re-fold (base_sha is a folded field). Jobs already at newBase are
// skipped (no churn / no spurious events). A ready job has no verdict or lease to
// invalidate, so this is a pure base advance — not a supersession.
func refreshReadyBaseTx(ctx context.Context, tx *sql.Tx, repo, mergedID, newBase string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, job_seq, lease_epoch FROM jobs
		 WHERE state = 'ready' AND kind = 'build' AND COALESCE(repo,'') = ?
		   AND id != ? AND COALESCE(base_sha,'') != ?`,
		repo, mergedID, newBase)
	if err != nil {
		return err
	}
	type row struct {
		id         string
		seq, epoch int
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.seq, &r.epoch); err != nil {
			rows.Close()
			return err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range rs {
		nextSeq := r.seq + 1
		ev := ledger.Event{
			JobID: r.id, JobSeq: nextSeq, Kind: ledger.KindBaseRefreshed,
			LeaseEpoch: r.epoch, Actor: "reconcile", CreatedAt: now,
			Payload: ledger.Payload{BaseSHA: newBase},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET base_sha = ?, updated_at = datetime('now') WHERE id = ?`, newBase, r.id); err != nil {
			return fmt.Errorf("refresh ready base %s: %w", r.id, err)
		}
		if err := setJobSeq(ctx, tx, r.id, nextSeq); err != nil {
			return err
		}
	}
	return nil
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
