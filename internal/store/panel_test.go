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

// TestPanelHeadMoveResetsRound: a head-establishing re-arm BETWEEN two panel approvals
// (a clean auto-rebase onto a moved base, or a conflict resolution) starts a FRESH round —
// the pre-move approval must NOT count toward the new head's quorum. Otherwise an N=2 panel
// could mint with a single distinct reviewer of the actual merged code: reviewer-1 approves
// head A, the base moves and the build is rebased to head B, reviewer-2 approves B, and the
// stale A-approval is miscounted as the 2nd vote on B. The round boundary is keyed on ALL
// head-establishing kinds (result_accepted, rebased, conflict_resolved), not result_accepted
// alone.
func TestPanelHeadMoveResetsRound(t *testing.T) {
	for _, kind := range []string{"rebased", "conflict_resolved"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			st := testutil.NewStore(t)
			src := store.DBFactSource{DB: st.DB}
			panel := job.Policy{RequiredReviewers: 2}
			now := time.Unix(3000, 0)

			driveToCodeReview(t, st, "mv", "h0", "b0")
			mustGreen(t, st, "mv", "h0", "b0")

			// reviewer 1 approves head A -> below N=2 -> accumulate (review_pending).
			if _, err := st.ReviewResult(ctx, src, panel, store.ReviewResultParams{
				JobID: "mv", Epoch: epochOf(t, st, "mv"), Claim: job.VerdictApproved, Now: now,
			}); err != nil {
				t.Fatalf("approve 1: %v", err)
			}

			// the reviewed head is re-established (rebase onto a moved base / conflict
			// resolution). Append that head-establishing event between the two approvals —
			// it re-arms review_pending at a NEW reviewed head, starting a fresh round.
			appendRawEvent(t, st, "mv", kind, "review_pending")

			// a DISTINCT reviewer approves the NEW head. With the round reset, this is only
			// the 1st distinct approval of head B -> must accumulate, NOT mint.
			if _, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
				JobID: "mv", LeaseID: "rl2", Identity: "reviewer2-mv", ModelFamily: "opus",
				Attested: []string{"role:code_reviewer", "model_family:opus"}, TTL: time.Minute, Now: now,
			}); err != nil {
				t.Fatalf("2nd reviewer claim: %v", err)
			}
			if _, err := st.ReviewResult(ctx, src, panel, store.ReviewResultParams{
				JobID: "mv", Epoch: epochOf(t, st, "mv"), Claim: job.VerdictApproved, Now: now,
			}); err != nil {
				t.Fatalf("approve 2 (post-%s): %v", kind, err)
			}
			j, _ := st.GetJob(ctx, "mv")
			if j.State == job.StateMergeable || j.Verdict != nil {
				t.Fatalf("a stale pre-%s approval must NOT count toward the new head's quorum; "+
					"state=%s verdict=%v (minted with one distinct reviewer of the moved head)", kind, j.State, j.Verdict)
			}
			if j.State != job.StateReviewPending {
				t.Fatalf("post-%s 1st distinct approval must accumulate; state=%s want review_pending", kind, j.State)
			}
		})
	}
}

// TestPanelHeadMoveAllowsSameReviewerOnNewRound: the same reviewer who approved the old
// head may review again after a head-establishing re-arm. The "one approval per reviewer"
// guard is scoped to one reviewed head; otherwise a single live reviewer can permanently
// strand a default single-reviewer job after project-out moves it from merge_handoff back to
// review_pending on a rebased head.
func TestPanelHeadMoveAllowsSameReviewerOnNewRound(t *testing.T) {
	for _, kind := range []string{"rebased", "conflict_resolved"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			st := testutil.NewStore(t)
			src := store.DBFactSource{DB: st.DB}
			panel := job.Policy{RequiredReviewers: 2}
			now := time.Unix(3000, 0)

			driveToCodeReview(t, st, "same", "h0", "b0")
			mustGreen(t, st, "same", "h0", "b0")

			if _, err := st.ReviewResult(ctx, src, panel, store.ReviewResultParams{
				JobID: "same", Epoch: epochOf(t, st, "same"), Claim: job.VerdictApproved, Now: now,
			}); err != nil {
				t.Fatalf("approve old head: %v", err)
			}

			appendRawEvent(t, st, "same", kind, "review_pending")

			if _, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
				JobID: "same", LeaseID: "rl-same-new-head", Identity: "reviewer-same", ModelFamily: "opus",
				Attested: []string{"role:code_reviewer", "model_family:opus"}, TTL: time.Minute, Now: now,
			}); err != nil {
				t.Fatalf("same reviewer must be able to review the new head after %s: %v", kind, err)
			}
		})
	}
}

// appendRawEvent appends a bare event of the given kind at the job's next ordinal — used to
// inject a head-establishing re-arm (rebased / conflict_resolved) into a panel round without
// driving the full GitHub-backed rebase machinery.
func appendRawEvent(t *testing.T, st *store.Store, jobID, kind, toState string) {
	t.Helper()
	ctx := context.Background()
	// the per-job ordinal tracker lives on the jobs row (loadJobTx reads jobs.job_seq); bump
	// both it and job_events in lockstep so the next real appendEvent lands at the right seq.
	var seq, epoch int
	if err := st.DB.QueryRowContext(ctx,
		`SELECT job_seq, lease_epoch FROM jobs WHERE id=?`, jobID).Scan(&seq, &epoch); err != nil {
		t.Fatalf("read job_seq for %s: %v", jobID, err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO job_events (job_id, job_seq, kind, from_state, to_state, lease_epoch, actor, payload)
		 VALUES (?,?,?,?,?,?, 'reconcile', '{}')`,
		jobID, seq+1, kind, "review_pending", toState, epoch); err != nil {
		t.Fatalf("append %s event: %v", kind, err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET job_seq=? WHERE id=?`, seq+1, jobID); err != nil {
		t.Fatalf("bump job_seq for %s: %v", jobID, err)
	}
}

func mustGreen(t *testing.T, st *store.Store, id, head, base string) {
	t.Helper()
	if err := st.SetReconciledFacts(context.Background(), id, store.ReconciledPR{
		Number: 42, HeadSHA: head, BaseSHA: base, CIGreen: true,
	}); err != nil {
		t.Fatalf("set green facts %s: %v", id, err)
	}
}
