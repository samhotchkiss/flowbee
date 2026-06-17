package store

import (
	"context"
	"database/sql"
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
//	    bug in any projection write can leave it disagreeing with a re-fold (#2217: a
//	    CI-fail bounce wrote the ledger's KindReviewBounced — whose fold yields
//	    role:eng_worker — but left the projection's required_capabilities as
//	    role:code_reviewer, so no builder could claim the `ready` job). For every
//	    leasable job this re-folds its ledger and, when the leaseability fields (state,
//	    role, required_capabilities) disagree, rewrites the projection to MATCH THE
//	    LEDGER. This is determinism-restoring (it makes projection == Fold(events), the
//	    system invariant) and self-heals the entire wedge CLASS, even from a future
//	    projection bug we haven't found yet.
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
			// (1) resync the projection to the ledger when leaseability fields diverge.
			if cur.State == folded.State &&
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
			// updated_at is a projection-only column (not folded onto job.Job); it is
			// touched on every lease op, so a legitimately-polled review/build looks
			// fresh and only a job with NO lease activity for the window trips this.
			var updated string
			if err := tx.QueryRowContext(ctx, `SELECT updated_at FROM jobs WHERE id=?`, id).Scan(&updated); err != nil {
				return err
			}
			ts, perr := time.Parse(rfc3339, updated)
			if perr != nil || now.Sub(ts) < stallAfter {
				return nil
			}
			ev := ledger.Event{
				JobID: id, JobSeq: seq + 1, Kind: ledger.KindStateChanged,
				FromState: cur.State, ToState: job.StateNeedsHuman, LeaseEpoch: cur.LeaseEpoch,
				Actor: "watchdog", CreatedAt: now,
			}
			if err := appendEvent(ctx, tx, ev); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE jobs SET state='needs_human', updated_at=datetime('now') WHERE id=?`, id); err != nil {
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
