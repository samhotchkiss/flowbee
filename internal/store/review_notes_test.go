package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestReviewFindingsCarriedToRebuild: a code-review bounce (changes_requested) carries
// the reviewer's findings onto the job (LastReviewNotes) so the rebuild's lease context
// surfaces them — the §F compounding-memory read side ("stops re-submitting patches that
// already failed review"). The carry-forward must fold (projection == Fold(events)).
func TestReviewFindingsCarriedToRebuild(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	src := store.DBFactSource{DB: st.DB}
	policy := job.Policy{}

	driveToCodeReview(t, st, "rj", "h0", "b0")
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs
		   SET patch_diff='diff --git a/hot.go b/hot.go',
		       declared_blast_radius='{"paths":["backend/hot.go"],"scope":""}',
		       reservation_paths='["backend/hot.go"]',
		       reservation_wide=1
		 WHERE id='rj'`); err != nil {
		t.Fatal(err)
	}
	const findings = "Fix: handle the nil case in parseConfig; add a test for empty input."
	if _, err := st.ReviewResult(ctx, src, policy, store.ReviewResultParams{
		JobID: "rj", Epoch: epochOf(t, st, "rj"),
		Claim: job.VerdictChangesRequested, Notes: findings, Now: time.Unix(3000, 0),
	}); err != nil {
		t.Fatalf("bounce: %v", err)
	}
	j, _ := st.GetJob(ctx, "rj")
	if j.State != job.StateReady {
		t.Fatalf("state=%s want ready (bounced to rebuild)", j.State)
	}
	if j.LastReviewNotes != findings {
		t.Fatalf("LastReviewNotes=%q, want the reviewer findings carried to the rebuild", j.LastReviewNotes)
	}
	var patch, declared, reservationPaths string
	var reservationWide int
	if err := st.DB.QueryRowContext(ctx, `
		SELECT patch_diff, declared_blast_radius, reservation_paths, reservation_wide
		  FROM jobs WHERE id='rj'`).Scan(&patch, &declared, &reservationPaths, &reservationWide); err != nil {
		t.Fatal(err)
	}
	if patch != "" || declared != "" || reservationPaths != "" || reservationWide != 0 {
		t.Fatalf("review bounce left stale build artifacts: patch=%q declared=%q reservation_paths=%q reservation_wide=%d",
			patch, declared, reservationPaths, reservationWide)
	}
	// the carry-forward is a projection field folded from the bounce event's payload.
	assertFoldMatchesProjection(t, st, "rj")
}

func TestAdoptedReviewBouncePreservesCumulativePatch(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	src := store.DBFactSource{DB: st.DB}

	driveToCodeReview(t, st, "adopted-rj", "adopted-head", "base")
	const cumulative = "diff --git a/original.go b/original.go\n--- a/original.go\n+++ b/original.go\n@@ -1 +1 @@\n-old\n+original change\n"
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET adopted=1, patch_diff=? WHERE id='adopted-rj'`, cumulative); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReviewResult(ctx, src, job.Policy{}, store.ReviewResultParams{
		JobID: "adopted-rj", Epoch: epochOf(t, st, "adopted-rj"),
		Claim: job.VerdictChangesRequested, Notes: "correct the edge case", Now: time.Unix(3000, 0),
	}); err != nil {
		t.Fatalf("bounce adopted PR: %v", err)
	}
	if got, err := st.JobPatchDiff(ctx, "adopted-rj"); err != nil || got != cumulative {
		t.Fatalf("adopted cumulative patch after bounce=%q err=%v, want preserved patch", got, err)
	}
}

func TestReviewMintsFromFreshFactsWhenJobHeadUnknown(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	src := store.DBFactSource{DB: st.DB}
	driveToCodeReview(t, st, "unknown-review-head", "reconciled-head", "base")
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET head_sha='' WHERE id='unknown-review-head'`); err != nil {
		t.Fatal(err)
	}
	mustGreen(t, st, "unknown-review-head", "reconciled-head", "base")

	resp, err := st.ReviewResult(ctx, src, job.Policy{}, store.ReviewResultParams{
		JobID: "unknown-review-head", Epoch: epochOf(t, st, "unknown-review-head"),
		Claim: job.VerdictApproved, Disposition: job.DispositionHandoff, Now: time.Unix(3000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Minted {
		t.Fatalf("fresh authoritative facts must mint when the job head is unknown: %+v", resp)
	}
}
