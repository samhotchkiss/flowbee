package project

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/job"
)

// TestAutonomousMergePinsReviewedHead: the merge call carries the reviewed head as the
// `sha` interlock. mergingJob seeds head_sha='head-sha'; the sender must pass exactly
// that to EnqueueMergeQueue so GitHub refuses to merge a head that moved after review.
func TestAutonomousMergePinsReviewedHead(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	sender.WithHistory(&fakeHistory{tip: "t", diffOut: diffAdding("docs/operating.md", "clean")}, "main")
	mergingJob(t, st, "j")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := fake.MergeHead(42); got != "head-sha" {
		t.Fatalf("merge pinned sha=%q, want the reviewed head %q", got, "head-sha")
	}
	if len(fake.Enqueued()) != 1 || fake.Enqueued()[0] != 42 {
		t.Fatalf("a head-matching clean merge must enqueue, got %v", fake.Enqueued())
	}
}

// TestAutonomousMergeDeferredWhenHeadMoved: the approve-then-push race. The content
// re-check is clean (the REVIEWED head is fine), but the PR's LIVE head moved to an
// unreviewed commit after the verdict. Because `merging` is non-supersedable, the SHA
// interlock is the only guard: GitHub 409s, and the sender routes to the human merge
// gate — never merging the unreviewed head, never blind-retrying it, never dead-lettering.
func TestAutonomousMergeDeferredWhenHeadMoved(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	sender.WithHistory(&fakeHistory{tip: "t", diffOut: diffAdding("docs/operating.md", "clean")}, "main")
	mergingJob(t, st, "j")
	// a commit landed on the feature branch after review: live head != reviewed head-sha.
	fake.SetHeadMoved(42, "unreviewed-head-2")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n := fake.Enqueued(); len(n) != 0 {
		t.Fatalf("the moved (unreviewed) head must NOT merge, got enqueued=%v", n)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff (head moved after review -> human gate)", j.State)
	}
}
