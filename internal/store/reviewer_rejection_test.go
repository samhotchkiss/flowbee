package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestSameReviewerSixRejectionsParksForHuman is the live proof of the per-review-node
// loop cap: when ONE review node requests changes on the same build job
// MaxReviewerRejections times, the job is parked for a human — and it fires BEFORE
// the cruder total-bounce backstop (total bounces stays under max_bounces), with the
// trigger stamped EscalationReviewerRejections so the operator queue is legible.
func TestSameReviewerSixRejectionsParksForHuman(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	src := store.DBFactSource{DB: st.DB}
	policy := job.Policy{}

	driveToCodeReview(t, st, "loopjob", "h0", "b0")
	const reviewer = "reviewer-loopjob" // the identity driveToCodeReview binds

	for i := 1; i <= job.MaxReviewerRejections; i++ {
		resp, err := st.ReviewResult(ctx, src, policy, store.ReviewResultParams{
			JobID: "loopjob", Epoch: epochOf(t, st, "loopjob"),
			Claim: job.VerdictChangesRequested, Now: time.Unix(int64(2000+i), 0),
		})
		if err != nil {
			t.Fatalf("rejection %d: %v", i, err)
		}
		j, err := st.GetJob(ctx, "loopjob")
		if err != nil {
			t.Fatalf("get job after rejection %d: %v", i, err)
		}

		if i < job.MaxReviewerRejections {
			// still iterating: a normal bounce re-arms the build stage.
			if j.State != job.StateReady {
				t.Fatalf("rejection %d by same reviewer: state=%s want ready (still under cap)", i, j.State)
			}
			if resp.JobState != string(job.StateReady) {
				t.Fatalf("rejection %d resp state=%s want ready", i, resp.JobState)
			}
			reReview(t, st, "loopjob", reviewer, i)
			continue
		}

		// the MaxReviewerRejections-th rejection by the SAME node parks for a human.
		if j.State != job.StateNeedsHuman {
			t.Fatalf("the %dth rejection by the SAME reviewer must park: state=%s", i, j.State)
		}
		if j.EscalationReason != string(job.EscalationReviewerRejections) {
			t.Fatalf("escalation_reason=%q want %q (legible per-reviewer trigger)",
				j.EscalationReason, job.EscalationReviewerRejections)
		}
		// the per-reviewer cap fired BEFORE the total backstop.
		if j.Bounces >= j.MaxBounces {
			t.Fatalf("per-reviewer cap should fire before the total backstop: bounces=%d max_bounces=%d",
				j.Bounces, j.MaxBounces)
		}
	}
}

// TestDistinctReviewersRideTheTotalBackstop is the contrast: rejections spread across
// DIFFERENT review nodes never trip the per-reviewer cap (each node's count stays
// low), so the job keeps iterating until the total max_bounces backstop catches it.
func TestDistinctReviewersRideTheTotalBackstop(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	src := store.DBFactSource{DB: st.DB}
	policy := job.Policy{}

	driveToCodeReview(t, st, "distjob", "h0", "b0")

	// a FRESH reviewer identity each round: no single node accumulates MaxReviewerRejections.
	// With max_bounces=9 the job survives past MaxReviewerRejections (6) rounds —
	// proof the per-reviewer cap did NOT fire.
	rounds := job.MaxReviewerRejections + 1 // 7 distinct reviewers, each rejecting once
	for i := 1; i <= rounds; i++ {
		j, err := st.GetJob(ctx, "distjob")
		if err != nil {
			t.Fatalf("get job round %d: %v", i, err)
		}
		if j.State != job.StateCodeReview {
			t.Fatalf("round %d: state=%s want code_review", i, j.State)
		}
		if _, err := st.ReviewResult(ctx, src, policy, store.ReviewResultParams{
			JobID: "distjob", Epoch: j.LeaseEpoch,
			Claim: job.VerdictChangesRequested, Now: time.Unix(int64(3000+i), 0),
		}); err != nil {
			t.Fatalf("round %d reject: %v", i, err)
		}
		j, _ = st.GetJob(ctx, "distjob")
		// every round here bounces (never the per-reviewer cap); the total backstop is 9.
		if j.State != job.StateReady {
			t.Fatalf("round %d: distinct reviewers should bounce (not park), state=%s reason=%s",
				i, j.State, j.EscalationReason)
		}
		reReview(t, st, "distjob", fmt.Sprintf("reviewer-dist-%d", i), i)
	}
}

// reReview re-arms a bounced (ready) build back to code_review, bound to the given
// reviewer identity — mimicking the rebuild→re-review cycle so the test can drive
// repeated rejections by a chosen review node.
func reReview(t *testing.T, st *store.Store, id, reviewer string, i int) {
	t.Helper()
	ctx := context.Background()
	now := time.Unix(int64(2100+i*10), 0)
	bl, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: id, LeaseID: fmt.Sprintf("bl-%s-%d", id, i), Identity: "builder-" + id, ModelFamily: "codex",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker", "model_family:codex"},
		TTL: time.Minute, Now: now,
	})
	if err != nil {
		t.Fatalf("re-claim build %d: %v", i, err)
	}
	if _, err := st.Result(ctx, store.ResultParams{JobID: id, Epoch: bl.Epoch, Now: now.Add(time.Second)}); err != nil {
		t.Fatalf("re-build result %d: %v", i, err)
	}
	if _, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: id, LeaseID: fmt.Sprintf("rl-%s-%d", id, i), Identity: reviewer, ModelFamily: "opus",
		Attested: []string{"role:code_reviewer", "model_family:opus"},
		TTL: time.Minute, Now: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("re-claim review %d: %v", i, err)
	}
}
