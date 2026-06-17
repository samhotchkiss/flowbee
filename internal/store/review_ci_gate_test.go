package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// seedReviewPending creates a job, drives it to review_pending with the reviewer
// gate, and (optionally) writes a domain_b_facts row. ciGreen controls whether the
// PR's reconciled CI is green.
func seedReviewPending(t *testing.T, st *store.Store, id string, now time.Time, withPR, ciGreen bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='review_pending', required_capabilities='["role:code_reviewer"]' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if withPR {
		green := 0
		if ciGreen {
			green = 1
		}
		if _, err := st.DB.ExecContext(ctx, `
			INSERT INTO domain_b_facts (job_id, pr_exists, pr_number, head_sha, base_sha, ci_green, merged, updated_at)
			VALUES (?, 1, 42, 'head', 'base', ?, 0, ?)`,
			id, green, now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReviewCandidatesExcludeCINotReady: a review whose CI is not green is NOT a
// lease candidate (it waits quietly until reconcile flips it green), while a
// CI-green review IS offered. This is the fix for the fleet-wide claim/release
// busy-wait — a not-ready review used to be offered, claimed, and instantly
// released by every poll for the whole CI window.
func TestReviewCandidatesExcludeCINotReady(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	seedReviewPending(t, st, "green", now, true, true)
	seedReviewPending(t, st, "pending", now, true, false)
	seedReviewPending(t, st, "nopr", now, false, false)

	cands, err := st.ReviewPendingCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.JobID] = true
		if !c.CIReady {
			t.Fatalf("candidate %s has CIReady=false — not-ready reviews must be filtered out", c.JobID)
		}
	}
	if !got["green"] {
		t.Fatalf("CI-green review must be a candidate; got %v", got)
	}
	if got["pending"] || got["nopr"] {
		t.Fatalf("CI-not-ready reviews must NOT be candidates; got %v", got)
	}
}

// TestWatchdogDoesNotEscalateReviewWaitingOnCI: a review_pending job with its PR
// open but CI not yet green is healthily blocked on Domain B, not a no-eligible
// dead-end. Even stalled past the window with a live fleet, the watchdog must leave
// it alone — escalating it on a stale updated_at would falsely page a human for a
// review that is simply waiting on a slow CI run. A review with NO PR yet stays
// escalatable (a genuine PR-creation wedge).
func TestWatchdogDoesNotEscalateReviewWaitingOnCI(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(100000, 0)

	seedReviewPending(t, st, "waiting", now, true, false) // PR open, CI pending
	seedReviewPending(t, st, "noprstuck", now, false, false)
	seedWorker(t, st, "feller-reviewer-1", now)

	// stall both: updated_at an hour in the past, no lease.
	stale := now.Add(-1 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET updated_at=? WHERE id IN ('waiting','noprstuck')`, stale); err != nil {
		t.Fatal(err)
	}

	rep, err := st.ReconcileStuck(ctx, now, 90*time.Second, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// only the no-PR review escalates; the CI-waiting one is left alone.
	if rep.Escalated != 1 {
		t.Fatalf("Escalated=%d, want 1 (no-PR review escalates, CI-waiting review does not)", rep.Escalated)
	}
	wj, _ := st.GetJob(ctx, "waiting")
	if wj.State != job.StateReviewPending {
		t.Fatalf("CI-waiting review state=%s, want review_pending (must not escalate)", wj.State)
	}
	nj, _ := st.GetJob(ctx, "noprstuck")
	if nj.State != job.StateNeedsHuman {
		t.Fatalf("no-PR stalled review state=%s, want needs_human", nj.State)
	}
}
