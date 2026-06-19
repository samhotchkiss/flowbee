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
	// the carry-forward is a projection field folded from the bounce event's payload.
	assertFoldMatchesProjection(t, st, "rj")
}
