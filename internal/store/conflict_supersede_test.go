package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestResolvedConflictHeadBaselinePreventsSupersede is the regression for a live wedge
// (found by two flowbee:build issues editing the same file): after a conflict_resolver
// force-pushes its resolution, the issue-branch head advances. If that resolved head is
// not recorded as the reconcile baseline, the very next sweep reads it as an unexpected
// SHA move and SUPERSEDES review_pending back to build — which rebuilds, re-pushes, and
// supersedes again: an organic-conflict resolve→supersede→rebuild loop. Recording the
// resolved head (ResolveConflictResult.PushedSHA) makes prevHead == the resolved head,
// so the resolution settles into review like any build.
func TestResolvedConflictHeadBaselinePreventsSupersede(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Unix(1000, 0)

	// the resolver rebases onto CURRENT main (newMain), which differs from the
	// pre-conflict base (oldBase) — so BOTH head and base advance.
	const oldBuildSHA, oldBase, resolvedSHA, newMain = "buildhead111", "oldbase00000", "resolvedhead2", "newmaintip000"

	putResolvingConflict(t, st, "rc", 0, now)
	// the resolver rebased onto newMain — RouteMergeConflict already advanced the job base.
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET base_sha=? WHERE id='rc'`, newMain); err != nil {
		t.Fatal(err)
	}
	// the PR/reconcile baseline still holds the OLD pre-conflict head + base.
	if err := st.UpsertDomainBFacts(ctx, "rc", job.DomainBFacts{
		PRExists: true, PRNumber: 7, HeadSHA: oldBuildSHA, BaseSHA: oldBase,
	}); err != nil {
		t.Fatalf("seed facts: %v", err)
	}

	// the resolver lands its resolution, reporting the pushed (resolved) head SHA.
	if _, err := st.ResolveConflictResult(ctx, store.ResolveConflictParams{
		JobID: "rc", Epoch: 1, ResolvedDiff: "diff --git a/x b/x\n",
		PushedSHA: resolvedSHA, Now: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	j, _ := st.GetJob(ctx, "rc")
	if j.State != job.StateReviewPending {
		t.Fatalf("after resolve state=%s want review_pending", j.State)
	}
	// the JOB records where Flowbee placed the branch (resolved head + rebased base);
	// reconcile's flowbeePlaced guard reads THESE (race-free, not domain_b_facts).
	if j.HeadSHA != resolvedSHA || j.BaseSHA != newMain {
		t.Fatalf("job head/base=%q/%q want resolved %q / rebased %q", j.HeadSHA, j.BaseSHA, resolvedSHA, newMain)
	}

	// the next reconcile sweep sees the PR at the resolved head + rebased base — the
	// flowbeePlaced guard recognises it as Flowbee's own advance, so the job STAYS in
	// review (the bug superseded it back to ready).
	if _, err := st.ApplyReconciledPR(ctx, "rc", store.ReconciledPR{
		Number: 7, HeadSHA: resolvedSHA, BaseSHA: newMain,
	}, now.Add(2*time.Second)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	j, _ = st.GetJob(ctx, "rc")
	if j.State != job.StateReviewPending {
		t.Fatalf("post-reconcile state=%s want review_pending (resolution must NOT be superseded)", j.State)
	}
}

// TestUnrecordedResolvedHeadWouldSupersede is the CONTRAST that pins the mechanism: if
// the resolved head is NOT recorded (the pre-fix behavior, simulated by an empty
// PushedSHA), the reconcile sweep reads the resolver's head advance as a move and
// supersedes the review back to build — the loop. This proves recording the head is
// exactly what closes the wedge.
func TestUnrecordedResolvedHeadWouldSupersede(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Unix(1000, 0)

	const buildSHA, resolvedSHA, mainSHA = "buildhead111", "resolvedhead2", "mainbase0000"

	putResolvingConflict(t, st, "rc", 0, now)
	if err := st.UpsertDomainBFacts(ctx, "rc", job.DomainBFacts{
		PRExists: true, PRNumber: 7, HeadSHA: buildSHA, BaseSHA: mainSHA,
	}); err != nil {
		t.Fatalf("seed facts: %v", err)
	}
	// resolve WITHOUT reporting the head (pre-fix): the baseline stays the old build head.
	if _, err := st.ResolveConflictResult(ctx, store.ResolveConflictParams{
		JobID: "rc", Epoch: 1, ResolvedDiff: "diff --git a/x b/x\n",
		PushedSHA: "", Now: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := reconHead(t, st, "rc"); got != buildSHA {
		t.Fatalf("without PushedSHA the baseline should stay the old build head, got %q", got)
	}
	// the reconcile sweep now sees the PR at the resolved head it never recorded -> a
	// move -> supersede back to build (the loop the fix prevents).
	if _, err := st.ApplyReconciledPR(ctx, "rc", store.ReconciledPR{
		Number: 7, HeadSHA: resolvedSHA, BaseSHA: mainSHA,
	}, now.Add(2*time.Second)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	j, _ := st.GetJob(ctx, "rc")
	if j.State != job.StateReady {
		t.Fatalf("unrecorded resolved head should supersede to ready (demonstrating the loop), got %s", j.State)
	}
}

// TestFlowbeePlacedSuppressesRebuildSupersede pins the general guard (the residual
// #113 churn): a review_pending job whose head_sha is the commit Flowbee just pushed
// (a rebuild or rebase-before-review) must NOT be superseded when reconcile observes
// that head — even if the reconcile baseline (domain_b_facts) still lags at the old
// head. An EXTERNAL head (not the job's) still supersedes.
// TestSupersedePreservesBaseOnEmptyIncomingBase: a supersede triggered by a head-only move
// must NOT blank base_sha when the sweep reports an empty base oid. The code explicitly
// anticipates "a head but an empty base oid" (reconcile.go), and a re-armed build with
// base_sha="" can't cut a worktree AND is skipped by the rebase sweep -> a needs_human dead
// end. supersedeTx keeps the prior base when the incoming one is empty (COALESCE/NULLIF),
// mirroring the KindSuperseded fold's existing `if BaseSHA != ""` guard (so projection==Fold).
func TestSupersedePreservesBaseOnEmptyIncomingBase(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Unix(1000, 0)

	const ownHead, mainBase, externalHead = "ownbuildhead0", "mainbase0000", "externalhead0"

	// seed WITH the base so the ledger carries it (fold-complete) — then the supersede's
	// keep-prior-base is verifiable against the fold, not just the projection.
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "sb", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: mainBase, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='review_pending', head_sha=? WHERE id='sb'`, ownHead); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDomainBFacts(ctx, "sb", job.DomainBFacts{
		PRExists: true, PRNumber: 9, HeadSHA: ownHead, BaseSHA: mainBase,
	}); err != nil {
		t.Fatal(err)
	}

	// an external head move with an EMPTY base oid -> supersede fires (real head move), but
	// the base must be preserved, not blanked.
	if _, err := st.ApplyReconciledPR(ctx, "sb", store.ReconciledPR{
		Number: 9, HeadSHA: externalHead, BaseSHA: "",
	}, now.Add(time.Second)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	j, _ := st.GetJob(ctx, "sb")
	if j.State != job.StateReady {
		t.Fatalf("external head move must supersede to ready, state=%s", j.State)
	}
	if j.BaseSHA != mainBase {
		t.Fatalf("supersede with an empty incoming base must KEEP the prior base; base_sha=%q want %q (a build with base_sha=\"\" can't cut a worktree and the rebase sweep skips it -> needs_human)", j.BaseSHA, mainBase)
	}
	assertFoldMatchesProjection(t, st, "sb")
}

func TestFlowbeePlacedSuppressesRebuildSupersede(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Unix(1000, 0)

	const newHead, oldHead, mainBase, externalHead = "rebuilthead00", "oldbuildhead0", "mainbase0000", "attackerhead0"

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "b", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// review_pending at the head Flowbee just pushed; baseline (domain_b_facts) lags.
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='review_pending', head_sha=?, base_sha=? WHERE id='b'`, newHead, mainBase); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDomainBFacts(ctx, "b", job.DomainBFacts{
		PRExists: true, PRNumber: 9, HeadSHA: oldHead, BaseSHA: mainBase,
	}); err != nil {
		t.Fatal(err)
	}

	// reconcile observes the PR at the job's own pushed head -> flowbeePlaced -> no supersede.
	if _, err := st.ApplyReconciledPR(ctx, "b", store.ReconciledPR{
		Number: 9, HeadSHA: newHead, BaseSHA: mainBase,
	}, now.Add(time.Second)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if j, _ := st.GetJob(ctx, "b"); j.State != job.StateReviewPending {
		t.Fatalf("Flowbee-placed head must not supersede, state=%s", j.State)
	}

	// an EXTERNAL head (not what Flowbee pushed) is a real move -> supersede.
	if _, err := st.ApplyReconciledPR(ctx, "b", store.ReconciledPR{
		Number: 9, HeadSHA: externalHead, BaseSHA: mainBase,
	}, now.Add(2*time.Second)); err != nil {
		t.Fatalf("reconcile2: %v", err)
	}
	if j, _ := st.GetJob(ctx, "b"); j.State != job.StateReady {
		t.Fatalf("external head move must supersede to ready, state=%s", j.State)
	}
}

func reconHead(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	var head string
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT COALESCE(head_sha,'') FROM domain_b_facts WHERE job_id=?`, id).Scan(&head); err != nil {
		t.Fatalf("read recon head: %v", err)
	}
	return head
}
