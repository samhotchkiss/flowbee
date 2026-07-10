package project

import (
	"context"
	"strings"
	"testing"

	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
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
	row, ok, err := st.NextPendingOutbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("failed-CI merge row should be abandoned after repair routing, still pending: %+v", row)
	}
}
