package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestReviewPendingGatedOnCIGreen pins the autonomous-fleet invariant: a review
// is offered only when reconciled CI is green (so a looping code_reviewer never
// approves a not-green PR, gets bounced, and re-arms the build in a thrash loop) —
// while a job with no facts row yet (pre-reconcile) is still offered.
func TestReviewPendingGatedOnCIGreen(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	mk := func(id string) {
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='review_pending' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	has := func(id string) bool {
		cands, err := st.ReviewPendingCandidates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cands {
			if c.JobID == id {
				return true
			}
		}
		return false
	}

	mk("nofacts")
	mk("red")
	mk("green")
	if err := st.UpsertDomainBFacts(ctx, "red", job.DomainBFacts{PRExists: true, CIGreen: false}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDomainBFacts(ctx, "green", job.DomainBFacts{PRExists: true, CIGreen: true}); err != nil {
		t.Fatal(err)
	}

	if !has("nofacts") {
		t.Fatal("a review_pending job with no facts row must still be offered (pre-reconcile)")
	}
	if has("red") {
		t.Fatal("ci_green=0 must be withheld from review (prevents the bounce/rebuild thrash)")
	}
	if !has("green") {
		t.Fatal("ci_green=1 must be a review candidate")
	}
}
