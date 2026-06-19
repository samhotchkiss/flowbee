package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestPanelSingleReviewerUnchanged: RequiredReviewers<=1 (the default) is the proven gate —
// the FIRST approval mints a verdict and the job goes mergeable. This locks the additive
// guarantee that the panel changes nothing for the single-reviewer case.
func TestPanelSingleReviewerUnchanged(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	src := store.DBFactSource{DB: st.DB}
	now := time.Unix(3000, 0)

	driveToCodeReview(t, st, "s1", "h0", "b0")
	mustGreen(t, st, "s1", "h0", "b0")
	if _, err := st.ReviewResult(ctx, src, job.Policy{}, store.ReviewResultParams{
		JobID: "s1", Epoch: epochOf(t, st, "s1"), Claim: job.VerdictApproved, Now: now,
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	j, _ := st.GetJob(ctx, "s1")
	if j.State != job.StateMergeable || j.Verdict == nil {
		t.Fatalf("single-reviewer gate: state=%s verdict=%v, want mergeable + minted", j.State, j.Verdict)
	}
	assertFoldMatchesProjection(t, st, "s1")
}

// TestPanelRequiresNDistinctApprovals: with RequiredReviewers=2, the FIRST approval does NOT
// mint — it accumulates (re-arms review_pending for the next reviewer). The SAME reviewer
// cannot cast the second approval (panel anti-affinity). A DISTINCT reviewer's approval is
// the Nth and mints. The accumulate path stays projection==Fold.
func TestPanelRequiresNDistinctApprovals(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	src := store.DBFactSource{DB: st.DB}
	panel := job.Policy{RequiredReviewers: 2}
	now := time.Unix(3000, 0)

	driveToCodeReview(t, st, "pj", "h0", "b0") // reviewer-pj bound in code_review
	mustGreen(t, st, "pj", "h0", "b0")

	// reviewer 1 approves -> below N=2 -> accumulate -> review_pending, NO mint.
	if _, err := st.ReviewResult(ctx, src, panel, store.ReviewResultParams{
		JobID: "pj", Epoch: epochOf(t, st, "pj"), Claim: job.VerdictApproved, Now: now,
	}); err != nil {
		t.Fatalf("approve 1: %v", err)
	}
	j, _ := st.GetJob(ctx, "pj")
	if j.State != job.StateReviewPending {
		t.Fatalf("after 1st approval state=%s, want review_pending (below N=2 must NOT mint)", j.State)
	}
	if j.Verdict != nil {
		t.Fatalf("no verdict may mint below N approvals; got %+v", j.Verdict)
	}
	assertFoldMatchesProjection(t, st, "pj")

	// the SAME reviewer cannot be the 2nd approval (panel anti-affinity).
	if _, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: "pj", LeaseID: "rl-same", Identity: "reviewer-pj", ModelFamily: "opus",
		Attested: []string{"role:code_reviewer", "model_family:opus"}, TTL: time.Minute, Now: now,
	}); err == nil {
		t.Fatal("a reviewer must not be able to cast TWO of the N panel approvals")
	}

	// a DISTINCT reviewer claims + approves -> N=2 reached -> mint -> mergeable.
	if _, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: "pj", LeaseID: "rl2", Identity: "reviewer2-pj", ModelFamily: "opus",
		Attested: []string{"role:code_reviewer", "model_family:opus"}, TTL: time.Minute, Now: now,
	}); err != nil {
		t.Fatalf("distinct reviewer claim: %v", err)
	}
	if _, err := st.ReviewResult(ctx, src, panel, store.ReviewResultParams{
		JobID: "pj", Epoch: epochOf(t, st, "pj"), Claim: job.VerdictApproved, Now: now,
	}); err != nil {
		t.Fatalf("approve 2: %v", err)
	}
	j, _ = st.GetJob(ctx, "pj")
	if j.State != job.StateMergeable || j.Verdict == nil {
		t.Fatalf("after 2nd distinct approval state=%s verdict=%v, want mergeable + minted", j.State, j.Verdict)
	}
	assertFoldMatchesProjection(t, st, "pj")
}

// TestPanelChangesRequestedResetsTheRound: an any-veto changes_requested at N=2 bounces the
// whole job to a rebuild (no partial consensus survives), and the next round starts fresh —
// the prior approval does NOT count toward the new round's quorum.
func TestPanelChangesRequestedResetsTheRound(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	src := store.DBFactSource{DB: st.DB}
	panel := job.Policy{RequiredReviewers: 2}
	now := time.Unix(3000, 0)

	driveToCodeReview(t, st, "rj", "h0", "b0")
	mustGreen(t, st, "rj", "h0", "b0")

	// 1st reviewer approves (accumulates), 2nd requests changes -> bounce to rebuild.
	if _, err := st.ReviewResult(ctx, src, panel, store.ReviewResultParams{
		JobID: "rj", Epoch: epochOf(t, st, "rj"), Claim: job.VerdictApproved, Now: now,
	}); err != nil {
		t.Fatalf("approve 1: %v", err)
	}
	if _, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: "rj", LeaseID: "rl2", Identity: "reviewer2-rj", ModelFamily: "opus",
		Attested: []string{"role:code_reviewer", "model_family:opus"}, TTL: time.Minute, Now: now,
	}); err != nil {
		t.Fatalf("2nd reviewer claim: %v", err)
	}
	if _, err := st.ReviewResult(ctx, src, panel, store.ReviewResultParams{
		JobID: "rj", Epoch: epochOf(t, st, "rj"), Claim: job.VerdictChangesRequested,
		Notes: "needs work", Now: now,
	}); err != nil {
		t.Fatalf("changes_requested: %v", err)
	}
	j, _ := st.GetJob(ctx, "rj")
	if j.State != job.StateReady {
		t.Fatalf("any-veto must bounce the whole job to rebuild; state=%s want ready", j.State)
	}
	assertFoldMatchesProjection(t, st, "rj")
}

// TestPanelAccumulateTracksReviewerHead: when a sub-threshold approval accumulates, the job's
// head_sha advances to the head the reviewer reported (its empty findings-commit). Without
// this the async reconcile reads that commit as a SHA move and supersedes the round. The fold
// tracks it too (projection == Fold), so a DR rebuild keeps the head current.
func TestPanelAccumulateTracksReviewerHead(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	src := store.DBFactSource{DB: st.DB}
	panel := job.Policy{RequiredReviewers: 2}
	now := time.Unix(3000, 0)

	driveToCodeReview(t, st, "ht", "h0", "b0")
	mustGreen(t, st, "ht", "h0", "b0")

	if _, err := st.ReviewResult(ctx, src, panel, store.ReviewResultParams{
		JobID: "ht", Epoch: epochOf(t, st, "ht"), Claim: job.VerdictApproved,
		ReviewerHead: "h-reviewer1-commit", Now: now,
	}); err != nil {
		t.Fatalf("approve 1: %v", err)
	}
	j, _ := st.GetJob(ctx, "ht")
	if j.State != job.StateReviewPending {
		t.Fatalf("state=%s want review_pending (accumulate)", j.State)
	}
	if j.HeadSHA != "h-reviewer1-commit" {
		t.Fatalf("accumulate must track the reviewer's pushed head (else reconcile supersedes the round); head=%q", j.HeadSHA)
	}
	assertFoldMatchesProjection(t, st, "ht")
}

func mustGreen(t *testing.T, st *store.Store, id, head, base string) {
	t.Helper()
	if err := st.SetReconciledFacts(context.Background(), id, store.ReconciledPR{
		Number: 42, HeadSHA: head, BaseSHA: base, CIGreen: true,
	}); err != nil {
		t.Fatalf("set green facts %s: %v", id, err)
	}
}
