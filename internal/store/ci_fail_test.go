package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestCIFailBouncesThenEscalates pins the CI-red handling: a review_pending build
// whose CI is definitively red bounces back to build (rebuild), and after
// max_bounces escalates to needs_human — never silently parks.
func TestCIFailBouncesThenEscalates(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// pin max_bounces=3 for this test so it asserts the ESCALATION MECHANISM, not the
	// shipped default (which is the higher total-bounce backstop now that the per-
	// review-node rejection cap is the primary review-loop trigger).
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET max_bounces=3 WHERE id='j'`); err != nil {
		t.Fatal(err)
	}
	toReview := func() {
		// mimic the real build->review_pending transition: the cap flips to the
		// reviewer role. The CI-fail bounce MUST reset it back to eng_worker, or the
		// re-armed `ready` build is unleaseable (no builder matches role:code_reviewer).
		if _, err := st.DB.ExecContext(ctx,
			`UPDATE jobs SET state='review_pending', required_capabilities='["role:code_reviewer"]' WHERE id='j'`); err != nil {
			t.Fatal(err)
		}
	}
	failPR := store.ReconciledPR{Number: 1, HeadSHA: "h1", BaseSHA: "b1", CIFailed: true}

	// max_bounces defaults to 3: three rebuilds, then escalate on the fourth.
	for i := 1; i <= 3; i++ {
		toReview()
		if _, err := st.ApplyReconciledPR(ctx, "j", failPR, now); err != nil {
			t.Fatal(err)
		}
		j, _ := st.GetJob(ctx, "j")
		if j.State != job.StateReady || j.Bounces != i {
			t.Fatalf("bounce %d: state=%s bounces=%d, want ready/%d", i, j.State, j.Bounces, i)
		}
		if j.Role != job.RoleEngWorker {
			t.Fatalf("bounce %d: role=%s, want eng_worker (re-armed for rebuild)", i, j.Role)
		}
		// the re-armed ready build MUST require the builder cap, not the reviewer cap —
		// else no worker can claim it and it wedges (the live #2217 stall).
		if len(j.RequiredCapabilities) != 1 || j.RequiredCapabilities[0] != "role:eng_worker" {
			t.Fatalf("bounce %d: required_capabilities=%v, want [role:eng_worker] (else unleaseable)", i, j.RequiredCapabilities)
		}
	}
	toReview()
	if _, err := st.ApplyReconciledPR(ctx, "j", failPR, now); err != nil {
		t.Fatal(err)
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateNeedsHuman {
		t.Fatalf("after max_bounces CI failures state=%s, want needs_human", j.State)
	}
}

// TestCIPendingDoesNotBounce: a not-yet-green (pending) PR must NOT bounce — only a
// DEFINITIVE failure does. CIFailed=false models pending.
func TestCIPendingDoesNotBounce(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "p", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='review_pending' WHERE id='p'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyReconciledPR(ctx, "p", store.ReconciledPR{Number: 2, HeadSHA: "h", BaseSHA: "b", CIFailed: false}, now); err != nil {
		t.Fatal(err)
	}
	j, _ := st.GetJob(ctx, "p")
	if j.State != job.StateReviewPending {
		t.Fatalf("pending CI must not bounce: state=%s want review_pending", j.State)
	}
}
