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

	const buildSHA, resolvedSHA, mainSHA = "buildhead111", "resolvedhead2", "mainbase0000"

	putResolvingConflict(t, st, "rc", 0, now)
	// the PR already exists at the OLD build head (the head that conflicted at merge).
	if err := st.UpsertDomainBFacts(ctx, "rc", job.DomainBFacts{
		PRExists: true, PRNumber: 7, HeadSHA: buildSHA, BaseSHA: mainSHA,
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
	// the resolved head is now the reconcile baseline.
	if got := reconHead(t, st, "rc"); got != resolvedSHA {
		t.Fatalf("domain_b_facts head=%q want the resolved head %q", got, resolvedSHA)
	}

	// the next reconcile sweep sees the PR at the resolved head — NO spurious move,
	// so the job STAYS in review (the bug would have superseded it back to ready).
	if _, err := st.ApplyReconciledPR(ctx, "rc", store.ReconciledPR{
		Number: 7, HeadSHA: resolvedSHA, BaseSHA: mainSHA,
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

func reconHead(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	var head string
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT COALESCE(head_sha,'') FROM domain_b_facts WHERE job_id=?`, id).Scan(&head); err != nil {
		t.Fatalf("read recon head: %v", err)
	}
	return head
}
