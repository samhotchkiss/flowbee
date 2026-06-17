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

// TestWatchdogToleratesSlowCIWithinWindow: a review_pending job blocked on a CI run
// that is still WITHIN the generous stall window is healthily waiting on Domain B —
// the watchdog must leave it alone (no false page for a slow CI run).
func TestWatchdogToleratesSlowCIWithinWindow(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(100000, 0)

	seedReviewPending(t, st, "slow", now, true, false) // PR open, CI pending
	seedWorker(t, st, "feller-reviewer-1", now)
	// updated_at 10 min ago — well within a 30 min window.
	recent := now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET updated_at=? WHERE id='slow'`, recent); err != nil {
		t.Fatal(err)
	}
	rep, err := st.ReconcileStuck(ctx, now, 90*time.Second, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Escalated != 0 {
		t.Fatalf("Escalated=%d, want 0 (slow CI within the window must not page)", rep.Escalated)
	}
	if j, _ := st.GetJob(ctx, "slow"); j.State != job.StateReviewPending {
		t.Fatalf("slow-CI review state=%s, want review_pending", j.State)
	}
}

// TestWatchdogEscalatesWedgedCI: a review whose CI never goes green for the WHOLE
// stall window is WEDGED (runner down / no workflow triggered), not merely slow — the
// watchdog must surface it to a human with a DISTINCT ci_stalled reason rather than
// let the review wait forever. A review with NO PR escalates as a generic wedge.
func TestWatchdogEscalatesWedgedCI(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(100000, 0)

	seedReviewPending(t, st, "wedged", now, true, false) // PR open, CI pending forever
	seedReviewPending(t, st, "noprstuck", now, false, false)
	seedWorker(t, st, "feller-reviewer-1", now)

	// stall both past the window: updated_at an hour in the past, no lease.
	stale := now.Add(-1 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET updated_at=? WHERE id IN ('wedged','noprstuck')`, stale); err != nil {
		t.Fatal(err)
	}

	rep, err := st.ReconcileStuck(ctx, now, 90*time.Second, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Escalated != 2 {
		t.Fatalf("Escalated=%d, want 2 (wedged-CI review + no-PR review both surface)", rep.Escalated)
	}
	wj, _ := st.GetJob(ctx, "wedged")
	if wj.State != job.StateNeedsHuman {
		t.Fatalf("wedged-CI review state=%s, want needs_human (no silent infinite wait)", wj.State)
	}
	if wj.EscalationReason != string(job.EscalationCIStalled) {
		t.Fatalf("wedged-CI escalation_reason=%q, want %q", wj.EscalationReason, job.EscalationCIStalled)
	}
	nj, _ := st.GetJob(ctx, "noprstuck")
	if nj.State != job.StateNeedsHuman {
		t.Fatalf("no-PR stalled review state=%s, want needs_human", nj.State)
	}
}
