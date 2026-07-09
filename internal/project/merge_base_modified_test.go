package project

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// TestMergeBaseModifiedRetriesNotDeadLetters: GitHub's retryable 405 "Base branch was
// modified" (a sibling PR merged first under concurrent merges) must be RETRIED by the
// drain — the row stays pending and the job stays in merging — NOT dead-lettered to
// needs_human and NOT routed to a conflict_resolver. This is the epic-flow concurrent
// merge case: every near-simultaneous merge's loser would otherwise strand at needs_human.
func TestMergeBaseModifiedRetriesNotDeadLetters(t *testing.T) {
	st, fake, sender, clk := newSender(t)
	// self-merge requires a mirror for the SHA-pin + content re-verify; wire a clean fake one.
	sender.WithHistory(&fakeHistory{tip: "h", diffOut: diffAdding("docs/x.md", "clean")}, "main")
	ctx := context.Background()

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	setMergingAuthorization(t, st, "j", "b", "h")
	fake.SetMergeBaseModified(91)
	if err := st.EnqueueOutbox(ctx, store.OutboxRow{
		JobID: "j", Action: store.ActionEnqueueMerge, HeadSHA: "h", Payload: `{"pr_number":91}`,
	}); err != nil {
		t.Fatal(err)
	}

	_, _ = sender.DrainOnce(ctx)

	j, _ := st.GetJob(ctx, "j")
	if j.State == job.StateNeedsHuman {
		t.Fatal("a base-modified merge dead-lettered the job to needs_human — it must retry")
	}
	if j.State == job.StateResolvingConflict {
		t.Fatal("a base-modified merge spuriously routed to a conflict_resolver — it must retry")
	}
	// the merge row is left pending for the next drain (after the base settles).
	row, ok, err := st.NextPendingOutbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || row.Action != store.ActionEnqueueMerge {
		t.Fatal("the base-modified merge row must remain pending for retry, not be consumed")
	}
	if row.Attempts == 0 {
		t.Fatal("attempts not bumped on the transient base-modified retry")
	}
}
