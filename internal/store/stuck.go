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

// StuckReport summarizes one forward-progress pass.
type StuckReport struct {
	Resynced  int // leasable jobs whose projection disagreed with the ledger and was corrected
	Escalated int // jobs stalled so long with a live fleet they were sent to needs_human
}

// ReconcileStuck is the forward-progress guarantee: no job stays permanently stuck.
//
//	(1) RESYNC — the jobs table is a READ MODEL folded from the canonical ledger; a
//	    bug in a projection write can leave it disagreeing with a re-fold (#2217: a
//	    CI-fail bounce wrote the ledger's KindReviewBounced — whose fold yields
//	    role:eng_worker — but left the projection's required_capabilities as
//	    role:code_reviewer, so no builder could claim the `ready` job). For a `ready`
//	    job this re-folds its ledger and, when the leaseability fields (role,
//	    required_capabilities) disagree, rewrites the projection to MATCH THE LEDGER. It
//	    is determinism-restoring (it makes projection == Fold(events), the invariant)
//	    and self-heals the wedge CLASS, even from a future projection bug.
//
//	    Scope: `ready` ONLY. `ready` is the capability-gated build-lease surface where
//	    the wedge bites, AND the fold is faithful there (job_created / bounce /
//	    supersede / deps_cleared all reproduce the build caps exactly). The review/spec/
//	    conflict gates capability-match too, but the fold does NOT reproduce their caps/
//	    role through the claim->release cycle (KindResultAccepted/KindReviewClaimed
//	    don't fold the reviewer cap the projection writes), so resyncing them to the
//	    fold would STRIP the gate. Those states can't wedge on a stale build cap and
//	    have their own no_eligible_worker alarm; leave them to it.
//	(2) ESCALATE — a job that re-folds clean but has still sat leasable and UNCLAIMED
//	    far longer than stallAfter, while live workers exist (so it is not merely a
//	    down fleet), is a no-eligible-worker dead-end. A real KindStateChanged event
//	    moves it to needs_human so a human always eventually sees it instead of it
//	    wedging silently forever.
//
// Legitimately-slow work is NOT escalated: a build/review being actively claimed
// touches updated_at on every lease op, so only a job with NO lease activity for the
// whole window trips the backstop. A down fleet is left alone — the fleet-health
// watchdog surfaces that; recovery is bringing the fleet back, not escalating waiters.
func (s *Store) ReconcileStuck(ctx context.Context, now time.Time, staleHB, stallAfter time.Duration) (StuckReport, error) {
	var rep StuckReport

	// liveness gate for escalation: only escalate when SOMEONE could have claimed it.
	// staleHB (a few missed heartbeats) decides "live"; stallAfter (much longer) is the
	// escalation window — never conflate the two.
	live := 0
	if roster, err := s.Roster(ctx, now, staleHB); err == nil {
		for _, w := range roster {
			if !w.StaleHB {
				live++
			}
		}
	}

	ids, err := s.leasableJobIDs(ctx)
	if err != nil {
		return rep, err
	}

	for _, id := range ids {
		// fold the canonical ledger for this job — the source of truth.
		events, err := s.LoadEvents(ctx, id)
		if err != nil {
			return rep, fmt.Errorf("load events %s: %w", id, err)
		}
		folded, err := ledger.Fold(events)
		if err != nil {
			return rep, fmt.Errorf("fold %s: %w", id, err)
		}

		err = s.tx(ctx, func(tx *sql.Tx) error {
			cur, seq, err := loadJobTx(ctx, tx, id)
			if err != nil {
				return err
			}
			// (1) resync the projection to the ledger when leaseability fields diverge —
			// `ready` only (see the doc comment: the fold is faithful for build caps but
			// not for the review/spec/conflict gates' caps).
			if cur.State == job.StateReady && cur.State == folded.State &&
				(cur.Role != folded.Role || !sameStrings(cur.RequiredCapabilities, folded.RequiredCapabilities)) {
				if _, err := tx.ExecContext(ctx, `
					UPDATE jobs SET role=?, required_capabilities=?, updated_at=datetime('now')
					 WHERE id=?`,
					string(folded.Role), marshalStrings(folded.RequiredCapabilities), id); err != nil {
					return fmt.Errorf("resync %s: %w", id, err)
				}
				rep.Resynced++
				return nil // it can be claimed now; give it a chance before escalating
			}
			// (2) escalation backstop: leasable, unclaimed, stalled past the window, with
			// a live fleet -> no eligible worker will ever take it. Surface to a human.
			if live == 0 || cur.LeaseID != "" {
				return nil
			}
			// A review_pending job with its PR open but CI not yet green is waiting on
			// Domain B (an external CI run). It is intentionally NOT offered to reviewers
			// until CI reconciles green (see ReviewPendingCandidates), so its updated_at no
			// longer advances on every poll. Within the (generous, ~4×lease_ttl) stall
			// window this is a healthy slow-CI wait — do NOT page. But it must not wait
			// FOREVER: a CI that never goes green in the WHOLE window is wedged (runner
			// down, no workflow triggered, perpetually pending), and a silent indefinite
			// review is a worse failure than a clear page — so past the window it escalates
			// with a DISTINCT ci_stalled reason (operator fixes CI / requeues, not hunts the
			// job). CI-red is bounced out of review_pending by reconcile, never escalated here.
			ciWedged := false
			if cur.State == job.StateReviewPending {
				if waiting, werr := reviewWaitingOnCITx(ctx, tx, id); werr == nil && waiting {
					ciWedged = true
				}
			}
			// updated_at is a projection-only column (not folded onto job.Job); it is
			// touched on every lease op, so a legitimately-polled review/build looks
			// fresh and only a job with NO lease activity for the window trips this.
			var updated string
			if err := tx.QueryRowContext(ctx, `SELECT updated_at FROM jobs WHERE id=?`, id).Scan(&updated); err != nil {
				return err
			}
			// updated_at is stored via datetime('now') (SQLite "2006-01-02 15:04:05" form), NOT
			// RFC3339 — parseDBTime handles both. Parsing it with rfc3339 used to always error,
			// so this whole escalation backstop was INERT in production: a leasable job no worker
			// could claim never escalated to needs_human, silently sitting forever (the exact
			// "permanently stuck" case this guard exists to prevent).
			ts, ok := parseDBTime(updated)
			if !ok || now.Sub(ts) < stallAfter {
				return nil // still within the generous window: a real build/review/CI cycle
			}
			// Always stamp a LEGIBLE reason. A CI-wedged review is ci_stalled; anything else
			// this backstop catches is a leasable-but-unclaimed job with a live fleet — a
			// no-eligible-worker capability/routing dead-end. A blank reason used to leak here,
			// which matched no self-clear exit and parked forever with no legible cause; naming
			// it lets the time-backstop eventually auto-cancel it and tells the operator why.
			reason := string(job.EscalationNoEligibleWorker)
			if ciWedged {
				reason = string(job.EscalationCIStalled)
			}
			ev := ledger.Event{
				JobID: id, JobSeq: seq + 1, Kind: ledger.KindStateChanged,
				FromState: cur.State, ToState: job.StateNeedsHuman, LeaseEpoch: cur.LeaseEpoch,
				Actor: "watchdog", CreatedAt: now,
				Payload: ledger.Payload{EscalationReason: reason},
			}
			if err := appendEvent(ctx, tx, ev); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE jobs SET state='needs_human',
				     escalation_reason = CASE WHEN ? <> '' THEN ? ELSE escalation_reason END,
				     updated_at=datetime('now') WHERE id=?`, reason, reason, id); err != nil {
				return fmt.Errorf("escalate %s: %w", id, err)
			}
			if err := setJobSeq(ctx, tx, id, seq+1); err != nil {
				return err
			}
			rep.Escalated++
			return nil
		})
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// reviewWaitingOnCITx reports whether a review_pending job is healthily blocked on
// Domain B: its PR exists, CI is not yet green, and it is not merged. Such a job is
// withheld from reviewers by ReviewPendingCandidates until CI reconciles, so the
// watchdog must not mistake its quiet (non-advancing updated_at) for a stall. A
// review with NO PR yet (pr_exists=0) is NOT covered — that can be a genuine
// PR-creation wedge, so it stays escalatable.
func reviewWaitingOnCITx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var prExists, ciGreen, merged int
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(pr_exists,0), COALESCE(ci_green,0), COALESCE(merged,0)
		  FROM domain_b_facts WHERE job_id=?`, id).Scan(&prExists, &ciGreen, &merged)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // no facts row yet -> no PR -> not a CI wait
	}
	if err != nil {
		return false, err
	}
	return prExists == 1 && ciGreen == 0 && merged == 0, nil
}

// NormalizeStrandedReadyBuilds is the self-heal safety net for a build job that landed in
// `ready` carrying STALE review/resolver capabilities or stale build-attempt artifacts.
// A `ready` build job MUST require role:eng_worker and MUST NOT carry the previous
// attempt's diff/blast-radius/reservation: those stale write-sets make reservation
// filtering treat old work as current and can serialize the whole fleet behind one
// active build. This direct projection repair aligns a diverged row with the canonical
// fresh-build re-arm; it does not invent a transition.
func (s *Store) NormalizeStrandedReadyBuilds(ctx context.Context, now time.Time) (int, error) {
	want := marshalStrings([]string{"role:eng_worker"})
	res, err := s.DB.ExecContext(ctx, `
		UPDATE jobs
		   SET role='eng_worker', stage='build', required_capabilities=?,
		       patch_diff='', declared_blast_radius='',
		       reservation_paths='', reservation_wide=0,
		       updated_at=datetime('now')
		 WHERE state='ready' AND kind='build'
		   AND (role != 'eng_worker' OR required_capabilities != ?
		        OR patch_diff != '' OR declared_blast_radius != ''
		        OR reservation_paths != '' OR reservation_wide != 0)`,
		want, want)
	if err != nil {
		return 0, fmt.Errorf("normalize stranded ready builds: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) leasableJobIDs(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id FROM jobs
		 WHERE state IN ('ready','review_pending','spec_authoring','spec_review','resolving_conflict')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
