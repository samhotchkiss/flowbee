package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestFoldBacklogToSpecReview drives the WHOLE spec-flow gate lifecycle — backlog ->
// promote -> spec_authoring -> author claim -> materialize -> spec_review — through the
// projection==Fold(events) invariant at every hop. Before the fold-class fix a rebuild
// from the ledger re-folded these as un-leasable stubs: KindBacklogged dropped
// kind/flow/role/caps (kind="", anyone-can-lease), and the spec gate states folded no
// role/caps at all (or the wrong code_reviewer caps), so the right agent could never
// claim. The strengthened assertFoldMatchesProjection now checks role/caps for the
// job's ACTUAL role + kind + spec_text, so this chain locks the whole class.
func TestFoldBacklogToSpecReview(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(2000, 0)

	// backlog seed: kind=spec / flow=spec / stage=backlog / role=spec_author / caps=[spec_author].
	if _, err := st.SeedBacklog(ctx, store.SeedBacklogParams{
		ID: "b", ChatRef: "c1", Priority: 3, TaskText: "build the thing",
		NeedsFullSpec: true, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if j, _ := st.GetJob(ctx, "b"); j.Kind != job.KindSpec || j.Role != job.RoleSpecAuthor || j.Priority != 3 {
		t.Fatalf("backlog seed: kind=%s role=%s priority=%d", j.Kind, j.Role, j.Priority)
	}
	assertFoldMatchesProjection(t, st, "b")

	// promote: backlog -> spec_authoring (the job becomes leasable by a spec_author).
	if final, err := st.PromoteBacklog(ctx, "b", now); err != nil || final != job.StateSpecAuthoring {
		t.Fatalf("promote -> %s err=%v, want spec_authoring", final, err)
	}
	assertFoldMatchesProjection(t, st, "b")

	// the spec_author claims the draft.
	if _, err := st.ClaimSpecAuthor(ctx, store.ClaimSpecAuthorParams{
		JobID: "b", LeaseID: "L1", Identity: "author-1", ModelFamily: "claude",
		Attested: []string{"role:spec_author"}, TTL: time.Minute, Now: now,
	}); err != nil {
		t.Fatalf("claim spec author: %v", err)
	}
	assertFoldMatchesProjection(t, st, "b")

	// materialize: spec_authoring -> spec_review, the authored spec_text retained + the
	// role flipped to spec_reviewer. The fold must carry spec_text AND the spec_reviewer
	// caps (so a reviewer — not a builder, not the author — can claim the gate).
	claimed, _ := st.GetJob(ctx, "b")
	const specMD = "# Spec\n\nBuild the thing, precisely.\n"
	if err := st.MaterializeSpec(ctx, store.MaterializeSpecParams{
		JobID: "b", ContentHash: "hash-v1", Version: 1, Markdown: specMD,
		Epoch: claimed.LeaseEpoch, Now: now,
	}); err != nil {
		t.Fatalf("materialize spec: %v", err)
	}
	rev, _ := st.GetJob(ctx, "b")
	if rev.State != job.StateSpecReview || rev.Role != job.RoleSpecReviewer || rev.SpecText != specMD {
		t.Fatalf("spec_review: state=%s role=%s spec_text=%q", rev.State, rev.Role, rev.SpecText)
	}
	assertFoldMatchesProjection(t, st, "b")
}

// TestFoldPROpenAndRequeueClearsHead locks the head_sha fold across the PR-open + requeue
// re-arm. KindPROpened now carries the head it stamped (read by reconcile's flowbeePlaced
// classifier), and the operator requeue clears head_sha + verdict; both must fold, or a
// rebuild-from-ledger blanks the head after PR-open (reconcile misclassifies the next
// sweep) or keeps a STALE head/verdict on a re-armed build.
func TestFoldPROpenAndRequeueClearsHead(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(2000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, RequiredCapabilities: []string{"role:eng_worker"},
		BaseSHA: "base", Now: now,
	}); err != nil {
		t.Fatal(err)
	}

	// PR opened: the head is stamped via the event, so the fold establishes it.
	if err := st.StampPRNumber(ctx, "j", 42, "headcafe", "base", now); err != nil {
		t.Fatalf("stamp pr: %v", err)
	}
	if j, _ := st.GetJob(ctx, "j"); j.HeadSHA != "headcafe" || j.PRNumber != 42 {
		t.Fatalf("after stamp: head=%q pr=%d", j.HeadSHA, j.PRNumber)
	}
	assertFoldMatchesProjection(t, st, "j")

	// strand it (needs_human) then requeue: the re-arm clears head_sha + verdict and
	// re-arms to ready/eng_worker. The fold must reproduce the cleared head (not the
	// stale "headcafe") — the divergence the helper's head_sha check now catches.
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='needs_human' WHERE id='j'`); err != nil {
		t.Fatal(err)
	}
	if final, err := st.RequeueJob(ctx, "j", false, now); err != nil || final != job.StateReady {
		t.Fatalf("requeue -> %s err=%v, want ready", final, err)
	}
	if j, _ := st.GetJob(ctx, "j"); j.HeadSHA != "" {
		t.Fatalf("requeue must clear head_sha, got %q", j.HeadSHA)
	}
	assertFoldMatchesProjection(t, st, "j")
}
