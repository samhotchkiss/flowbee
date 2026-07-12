package project

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func claimReplacementRepair(t *testing.T, st *store.Store, id string, now time.Time) int {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs
		   SET state='ready', role='eng_worker', stage='build',
		       required_capabilities='["role:eng_worker"]', enqueued_at=?
		 WHERE id=?`, now.Format(time.RFC3339Nano), id); err != nil {
		t.Fatalf("arm replacement repair: %v", err)
	}
	lease, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: id, LeaseID: "replacement-lease", Identity: "builder", ModelFamily: "gpt",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker"}, TTL: time.Minute, Now: now,
	})
	if err != nil {
		t.Fatalf("claim replacement repair: %v", err)
	}
	return lease.Epoch
}

func containsCandidate(candidates []string, want string) bool {
	for _, candidate := range candidates {
		if candidate == want {
			return true
		}
	}
	return false
}

func TestReplacementPRIsBoundAtomicallyAndWithheldUntilReconciled(t *testing.T) {
	st, fake, sender, clk := newSender(t)
	ctx := context.Background()
	now := clk.Now()
	const originalPR = 4190
	original := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-a\n+b\n"
	id, _, err := st.AdoptPRForReviewWithHeadRef(ctx, "russ", originalPR, "base", "old-head", "foreign/hotfix", original, false, false, true, false, now, now)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	epoch := claimReplacementRepair(t, st, id, now.Add(time.Minute))
	cumulative := original + "diff --git a/limit_test.go b/limit_test.go\n--- a/limit_test.go\n+++ b/limit_test.go\n@@ -0,0 +1 @@\n+zero limit\n"
	if _, err := st.Result(ctx, store.ResultParams{
		JobID: id, Epoch: epoch, Now: now.Add(2 * time.Minute),
		PushedSHA: "replacement-head", PushedBranch: store.PRBranch(id), PatchDiff: cumulative,
	}); err != nil {
		t.Fatalf("replacement result: %v", err)
	}

	before, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get pre-open replacement: %v", err)
	}
	if before.PRNumber != 0 || before.HeadSHA != "" || before.HeadRef != store.PRBranch(id) || before.PendingRepairHeadSHA != "replacement-head" {
		t.Fatalf("pre-open replacement binding=%+v", before)
	}
	assertReplacementFold(t, st, id, before)

	if n, err := sender.DrainOnce(ctx); err != nil || n != 2 {
		t.Fatalf("replacement open/link drain n=%d err=%v", n, err)
	}
	bound, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get bound replacement: %v", err)
	}
	if bound.PRNumber == 0 || bound.HeadRef != store.PRBranch(id) || bound.HeadSHA != "" || bound.PendingRepairHeadSHA != "replacement-head" {
		t.Fatalf("replacement was not atomically rebound: %+v", bound)
	}
	assertReplacementFold(t, st, id, bound)

	comments := fake.Comments(originalPR)
	if len(comments) != 1 || comments[0] != "Superseded by replacement PR #"+strconv.Itoa(bound.PRNumber)+"." {
		t.Fatalf("original PR link=%v, want replacement link", comments)
	}

	if cands, err := st.ReviewPendingCandidates(ctx); err != nil {
		t.Fatalf("candidates before reconcile: %v", err)
	} else {
		for _, c := range cands {
			if c.JobID == id {
				t.Fatal("replacement became reviewable before replacement PR facts reconciled")
			}
		}
	}
	if _, err := st.ApplyReconciledPR(ctx, id, store.ReconciledPR{
		Number: bound.PRNumber, HeadSHA: "replacement-head", BaseSHA: "base", CIGreen: true,
		UpdatedAt: now.Add(3 * time.Minute),
	}, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("reconcile replacement PR: %v", err)
	}
	after, err := st.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get reconciled replacement: %v", err)
	}
	if after.HeadSHA != "replacement-head" || after.PendingRepairHeadSHA != "" {
		t.Fatalf("reconciled replacement head/pending=%q/%q", after.HeadSHA, after.PendingRepairHeadSHA)
	}
	assertReplacementFold(t, st, id, after)
	candidates, err := st.ReviewPendingCandidates(ctx)
	if err != nil {
		t.Fatalf("candidates after reconcile: %v", err)
	}
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.JobID)
	}
	if !containsCandidate(ids, id) {
		t.Fatalf("replacement must become reviewable only at reconciled visible head: %+v", candidates)
	}
}

func assertReplacementFold(t *testing.T, st *store.Store, id string, want job.Job) {
	t.Helper()
	events, err := st.LoadEvents(context.Background(), id)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	folded, err := ledger.Fold(events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded.PRNumber != want.PRNumber || folded.HeadSHA != want.HeadSHA || folded.HeadRef != want.HeadRef ||
		folded.PendingRepairHeadSHA != want.PendingRepairHeadSHA {
		t.Fatalf("fold replacement binding=%+v, want projection=%+v", folded, want)
	}
}
