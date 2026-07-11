package project

import (
	"context"
	"strings"
	"testing"

	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestAutonomousMergeBlocksWhileRequiredCheckInProgress(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	sender.WithHistory(&fakeHistory{tip: "t", diffOut: diffAdding("docs/operating.md", "clean")}, "main")
	mergingJob(t, st, "j")
	fake.SetBranchProtection("main", gh.Protection{RequiredChecks: []string{"backend shard 2"}})
	fake.SetPR(gh.PullRequest{
		Number: 42, HeadRefOid: "head-sha", BaseRefOid: "base-sha",
		CIRollup: gh.CIPending,
	})

	if n, err := sender.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("pending required check must not drain merge row, n=%d err=%v", n, err)
	}
	if got := fake.Enqueued(); len(got) != 0 {
		t.Fatalf("merge was enqueued while a required check was pending: %v", got)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMerging {
		t.Fatalf("state=%s, want still merging while CI runs", j.State)
	}
	row, ok, err := st.NextPendingOutbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || row.Action != store.ActionEnqueueMerge || row.Attempts != 0 {
		t.Fatalf("merge row should remain pending without retry-budget burn, row=%+v ok=%v", row, ok)
	}

	fake.SetPR(gh.PullRequest{
		Number: 42, HeadRefOid: "head-sha", BaseRefOid: "base-sha",
		CIRollup: gh.CISuccess, PassedChecks: []string{"backend shard 2"},
	})
	if n, err := sender.DrainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("green required check should allow merge, n=%d err=%v", n, err)
	}
	if got := fake.Enqueued(); len(got) != 1 || got[0] != 42 {
		t.Fatalf("merge should enqueue only after required check succeeds, got %v", got)
	}
}

func TestAutonomousMergeRoutesRepairWhenRequiredCheckFailsAfterStaleGreen(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	sender.WithHistory(&fakeHistory{tip: "t", diffOut: diffAdding("docs/operating.md", "clean")}, "main")
	mergingJob(t, st, "j")
	fake.SetBranchProtection("main", gh.Protection{RequiredChecks: []string{"backend shard 2"}})
	fake.SetPR(gh.PullRequest{
		Number: 42, HeadRefOid: "head-sha", BaseRefOid: "base-sha",
		CIRollup: gh.CIFailure, FailingChecks: []string{"backend shard 2"},
		FailingCheckURLs: map[string]string{
			"backend shard 2": "https://github.com/acme/api/actions/runs/123",
		},
	})

	if n, err := sender.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("failed required check should route repair without sending merge, n=%d err=%v", n, err)
	}
	if got := fake.Enqueued(); len(got) != 0 {
		t.Fatalf("merge was enqueued after live required check failed: %v", got)
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateReady || j.Verdict != nil {
		t.Fatalf("job state/verdict=%s/%+v, want repair-ready with stale verdict cleared", j.State, j.Verdict)
	}
	for _, want := range []string{"backend shard 2", "https://github.com/acme/api/actions/runs/123"} {
		if !strings.Contains(j.LastCIFailures, want) {
			t.Fatalf("last_ci_failures=%q missing %q", j.LastCIFailures, want)
		}
	}
	events, err := st.LoadEvents(ctx, "j")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	folded, err := ledger.Fold(events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded.LastCIFailures != j.LastCIFailures {
		t.Fatalf("fold last_ci_failures=%q != projection %q; repair URL must survive replay",
			folded.LastCIFailures, j.LastCIFailures)
	}
	row, ok, err := st.NextPendingOutbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("failed-CI merge row should be abandoned after repair routing, still pending: %+v", row)
	}
}

// TestAutonomousMergeBlocksSubstantiveNonRequiredFailure is the #4165 regression: the
// repo ruleset marks only a thin check required ("Migration version guard"), the head
// passes it, but a NON-required substantive shard ("backend shard 3") is red. The old
// required-only gate self-merged; the merge must now route to CI repair instead.
func TestAutonomousMergeBlocksSubstantiveNonRequiredFailure(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	sender.WithHistory(&fakeHistory{tip: "t", diffOut: diffAdding("docs/operating.md", "clean")}, "main")
	mergingJob(t, st, "j")
	fake.SetBranchProtection("main", gh.Protection{RequiredChecks: []string{"Migration version guard"}})
	fake.SetPR(gh.PullRequest{
		Number: 42, HeadRefOid: "head-sha", BaseRefOid: "base-sha",
		CIRollup:         gh.CIFailure,
		CIHasRealSuccess: true,
		PassedChecks:     []string{"Migration version guard"},
		FailingChecks:    []string{"backend shard 3"},
		FailingCheckURLs: map[string]string{
			"backend shard 3": "https://github.com/acme/api/actions/runs/999",
		},
	})

	if n, err := sender.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("red non-required substantive shard must not enqueue merge, n=%d err=%v", n, err)
	}
	if got := fake.Enqueued(); len(got) != 0 {
		t.Fatalf("merge enqueued despite a red substantive non-required shard: %v", got)
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateReady || j.Verdict != nil {
		t.Fatalf("job state/verdict=%s/%+v, want repair-ready with stale verdict cleared", j.State, j.Verdict)
	}
	if !strings.Contains(j.LastCIFailures, "backend shard 3") {
		t.Fatalf("last_ci_failures=%q missing the substantive shard", j.LastCIFailures)
	}
}

// TestAutonomousMergeToleratesCosmeticNonRequiredFailure guards the other side of the
// #4165 fix: a red NON-required cosmetic gate (a linter) must NOT freeze the board when
// every required check is green, so the approved PR still self-merges.
func TestAutonomousMergeToleratesCosmeticNonRequiredFailure(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	sender.WithHistory(&fakeHistory{tip: "t", diffOut: diffAdding("docs/operating.md", "clean")}, "main")
	mergingJob(t, st, "j")
	fake.SetBranchProtection("main", gh.Protection{RequiredChecks: []string{"Migration version guard"}})
	fake.SetPR(gh.PullRequest{
		Number: 42, HeadRefOid: "head-sha", BaseRefOid: "base-sha",
		CIRollup:         gh.CIFailure,
		CIHasRealSuccess: true,
		PassedChecks:     []string{"Migration version guard"},
		FailingChecks:    []string{"lint / eslint"},
	})

	if n, err := sender.DrainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("cosmetic-only non-required failure should still merge, n=%d err=%v", n, err)
	}
	if got := fake.Enqueued(); len(got) != 1 || got[0] != 42 {
		t.Fatalf("approved PR with a green required check should merge past a cosmetic lint, got %v", got)
	}
}
