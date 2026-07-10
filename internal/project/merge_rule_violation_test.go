package project

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// TestRequiredCheckExpectedRetriesThenMerges: GitHub can reject a reviewed merge with a
// ruleset 405 while a required check is still "expected". That row must stay pending and
// retry, not dead-letter the job to needs_human; once the fake clears, the merge drains.
func TestRequiredCheckExpectedRetriesThenMerges(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	sender.WithHistory(&fakeHistory{tip: "head-sha", diffOut: diffAdding("docs/x.md", "clean")}, "main")
	mergingJob(t, st, "j")
	setLiveGreenPR(fake, 42, "base-sha", "head-sha")
	fake.SetMergeRuleViolationPending(42, 1, false)

	_, _ = sender.DrainOnce(ctx)
	if j, _ := st.GetJob(ctx, "j"); j.State == job.StateNeedsHuman {
		t.Fatal("required-check-pending ruleset 405 dead-lettered the job to needs_human")
	}
	if len(fake.Enqueued()) != 0 {
		t.Fatalf("first merge attempt should be rejected before enqueue succeeds, got %v", fake.Enqueued())
	}
	if got := fake.UpdatedBranches(); len(got) != 0 {
		t.Fatalf("pending required check must wait for CI, not update-branch: %v", got)
	}
	row, ok, err := st.NextPendingOutbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || row.Action != store.ActionEnqueueMerge || row.Attempts == 0 {
		t.Fatalf("merge row must remain pending with bumped attempts, row=%+v ok=%v", row, ok)
	}

	if n, err := sender.DrainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("second drain should merge after the rule clears, n=%d err=%v", n, err)
	}
	if got := fake.Enqueued(); len(got) != 1 || got[0] != 42 {
		t.Fatalf("merge should succeed on retry, enqueued=%v", got)
	}
}

// TestBehindRuleViolationUpdatesBranchAndRetries: if the ruleset 405 says the branch is
// behind, project-out fast-forwards it best-effort and still leaves the merge row pending
// for the normal retry budget instead of escalating.
func TestBehindRuleViolationUpdatesBranchAndRetries(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	sender.WithHistory(&fakeHistory{tip: "head-sha", diffOut: diffAdding("docs/x.md", "clean")}, "main")
	mergingJob(t, st, "j")
	setLiveGreenPR(fake, 42, "base-sha", "head-sha")
	fake.SetMergeRuleViolationPending(42, 1, true)

	_, _ = sender.DrainOnce(ctx)
	if j, _ := st.GetJob(ctx, "j"); j.State == job.StateNeedsHuman {
		t.Fatal("behind ruleset 405 dead-lettered the job to needs_human")
	}
	if got := fake.UpdatedBranches(); len(got) != 1 || got[0] != 42 {
		t.Fatalf("behind ruleset violation should update-branch PR 42, got %v", got)
	}
	row, ok, err := st.NextPendingOutbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || row.Action != store.ActionEnqueueMerge || row.Attempts == 0 {
		t.Fatalf("merge row must remain pending with bumped attempts, row=%+v ok=%v", row, ok)
	}

	if n, err := sender.DrainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("second drain should retry the merge after update-branch, n=%d err=%v", n, err)
	}
	if got := fake.Enqueued(); len(got) != 1 || got[0] != 42 {
		t.Fatalf("merge should succeed on retry, enqueued=%v", got)
	}
}
