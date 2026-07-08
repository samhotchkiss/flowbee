package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestAdoptPRForReview covers the targeted single-PR adoption (`flowbee adopt <pr>`):
// a pre-existing PR Flowbee did not originate is imported as an opted-in adopted
// code_reviewer job in review_pending, with its Domain-B facts reconciled — and the
// import is idempotent.
func TestAdoptPRForReview(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	id, err := st.AdoptPRForReview(ctx, "russ", 4242, "base-sha", "head-sha", false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if id == "" {
		t.Fatal("expected a new adopted job id")
	}

	j, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.State != job.StateReviewPending {
		t.Fatalf("adopted PR state=%q, want review_pending", j.State)
	}
	if j.Role != job.RoleCodeReviewer {
		t.Fatalf("adopted PR role=%q, want code_reviewer", j.Role)
	}
	if j.PRNumber != 4242 {
		t.Fatalf("adopted PR number=%d, want 4242", j.PRNumber)
	}
	// repo MUST be set — else project-OUT's per-repo outbox drain strands the merge.
	if j.Repo != "russ" {
		t.Fatalf("adopted PR repo=%q, want russ (empty repo strands the merge in multi-repo)", j.Repo)
	}
	// it must be opted-in (NOT quiescent) — project-out has to render it so the
	// reviewer is actually offered the work.
	quiescent, err := st.IsQuiescent(ctx, id)
	if err != nil {
		t.Fatalf("quiescent check: %v", err)
	}
	if quiescent {
		t.Fatal("an adopted-for-review PR must be opted in, not quiescent")
	}

	// idempotent: re-adopting the same PR is a no-op ("" id), no duplicate job.
	again, err := st.AdoptPRForReview(ctx, "russ", 4242, "base-sha", "head-sha", false, true, false, now, now)
	if err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
	if again != "" {
		t.Fatalf("re-adopting a tracked PR must be a no-op, got new id %q", again)
	}
}

// TestAdoptPRForReviewSkipsOriginatedPR: a PR Flowbee ALREADY originated (its own
// build job carries the pr_number) must not be re-adopted — adoption is only for
// foreign PRs. The idempotency guard keys on pr_number regardless of how the job
// was created.
func TestAdoptPRForReviewSkipsOriginatedPR(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	// seed a normal build job and stamp it with a PR number (as project-out does on
	// PR creation), simulating a Flowbee-originated PR.
	issue := 77
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "orig-1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, IssueNumber: &issue, Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.StampPRNumber(ctx, "orig-1", 555, "head", "base", now); err != nil {
		t.Fatalf("stamp pr: %v", err)
	}

	id, err := st.AdoptPRForReview(ctx, "russ", 555, "base", "head", false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if id != "" {
		t.Fatalf("must not adopt a PR Flowbee already originated, got id %q", id)
	}
}
