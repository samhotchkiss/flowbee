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

	const patch = "diff --git a/x.go b/x.go\nindex 1111111..2222222 100644\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\n"
	id, err := st.AdoptPRForReview(ctx, "russ", 4242, "base-sha", "head-sha", patch, false, false, true, false, now, now)
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
	if d, err := st.JobPatchDiff(ctx, id); err != nil || d != patch {
		t.Fatalf("adopted PR patch_diff=%q err=%v, want authoritative patch", d, err)
	}
	if j.DiffEmpty {
		t.Fatal("nonempty adopted PR diff must not be marked empty")
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
	again, err := st.AdoptPRForReview(ctx, "russ", 4242, "base-sha", "head-sha", patch, false, false, true, false, now, now)
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
// was created, within the same repo scope.
func TestAdoptPRForReviewSkipsOriginatedPR(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	// seed a normal build job and stamp it with a PR number (as project-out does on
	// PR creation), simulating a Flowbee-originated PR.
	issue := 77
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "orig-1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, IssueNumber: &issue, Repo: "russ", Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.StampPRNumber(ctx, "orig-1", 555, "head", "base", now); err != nil {
		t.Fatalf("stamp pr: %v", err)
	}

	id, err := st.AdoptPRForReview(ctx, "russ", 555, "base", "head", "diff --git a/x b/x\n", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if id != "" {
		t.Fatalf("must not adopt a PR Flowbee already originated, got id %q", id)
	}
}

func TestAdoptPRForReviewRefreshesLegacyAndHeadMove(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	id, err := st.AdoptPRForReview(ctx, "russ", 99, "base1", "head1", "", false, false, true, false, now, now)
	if err != nil || id == "" {
		t.Fatalf("legacy seed adopt id=%q err=%v", id, err)
	}
	if d, _ := st.JobPatchDiff(ctx, id); d != "" {
		t.Fatalf("legacy setup patch=%q, want empty", d)
	}
	again, err := st.AdoptPRForReview(ctx, "russ", 99, "base1", "head1", "diff --git a/a b/a\n", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("backfill adopt: %v", err)
	}
	if again != "" {
		t.Fatalf("backfill should not duplicate, got id %q", again)
	}
	if d, _ := st.JobPatchDiff(ctx, id); d != "diff --git a/a b/a\n" {
		t.Fatalf("backfilled patch=%q", d)
	}

	_, err = st.AdoptPRForReview(ctx, "russ", 99, "base1", "head2", "diff --git a/b b/b\n", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("head refresh adopt: %v", err)
	}
	j, _ := st.GetJob(ctx, id)
	if j.HeadSHA != "head2" {
		t.Fatalf("head_sha=%q, want head2", j.HeadSHA)
	}
	if d, _ := st.JobPatchDiff(ctx, id); d != "diff --git a/b b/b\n" {
		t.Fatalf("refreshed patch=%q", d)
	}
}

func TestAdoptPRForReviewScopesPRNumbersByRepo(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	idA, err := st.AdoptPRForReview(ctx, "core", 4078, "base-a", "head-a", "diff --git a/core b/core\n", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt core: %v", err)
	}
	idB, err := st.AdoptPRForReview(ctx, "web", 4078, "base-b", "head-b", "diff --git a/web b/web\n", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt web: %v", err)
	}
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("repo-scoped PRs should create distinct jobs, got %q and %q", idA, idB)
	}
	if d, _ := st.JobPatchDiff(ctx, idA); d != "diff --git a/core b/core\n" {
		t.Fatalf("core patch=%q", d)
	}
	if d, _ := st.JobPatchDiff(ctx, idB); d != "diff --git a/web b/web\n" {
		t.Fatalf("web patch=%q", d)
	}
}

func TestAdoptPRForReviewRecordsExplicitEmptyDiff(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	id, err := st.AdoptPRForReview(ctx, "russ", 12, "same", "same", "", true, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt empty: %v", err)
	}
	j, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get empty job: %v", err)
	}
	if !j.DiffEmpty {
		t.Fatal("explicit empty adopted PR must set DiffEmpty")
	}
	if d, _ := st.JobPatchDiff(ctx, id); d != "" {
		t.Fatalf("empty patch_diff=%q, want empty", d)
	}
}

func TestAdoptedPRMissingDiffIsNotReviewCandidate(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(9000, 0)

	id, err := st.AdoptPRForReview(ctx, "russ", 13, "base", "head", "", false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt legacy missing diff: %v", err)
	}
	cands, err := st.ReviewPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("review candidates: %v", err)
	}
	for _, c := range cands {
		if c.JobID == id {
			t.Fatalf("legacy adopted PR with missing diff must be withheld from review candidates: %+v", cands)
		}
	}

	if _, err := st.AdoptPRForReview(ctx, "russ", 13, "base", "head", "diff --git a/x b/x\n", false, false, true, false, now, now); err != nil {
		t.Fatalf("backfill diff: %v", err)
	}
	cands, err = st.ReviewPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("review candidates after backfill: %v", err)
	}
	for _, c := range cands {
		if c.JobID == id {
			return
		}
	}
	t.Fatalf("backfilled adopted PR should become review candidate: %+v", cands)
}
