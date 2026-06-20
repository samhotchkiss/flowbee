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

// TestAutonomousMergeDeferredWhenHeadMoved: the approve-then-push race with a REAL content
// change. The PR's live head moved to an unreviewed commit AND its base..head diff differs
// from what was reviewed, so the content-equality recovery (russ #214) correctly declines
// and the sender routes to the human merge gate — never merging the unreviewed change.
func TestAutonomousMergeDeferredWhenHeadMoved(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	// live head "moved-head" carries a DIFFERENT diff than the reviewed "head-sha".
	sender.WithHistory(&fakeHistory{
		tip:     "moved-head",
		diffOut: diffAdding("docs/operating.md", "clean"),
		diffByHead: map[string]string{
			"head-sha":   diffAdding("docs/operating.md", "reviewed line"),
			"moved-head": diffAdding("docs/operating.md", "UNREVIEWED line"),
		},
	}, "main")
	mergingJob(t, st, "j")
	fake.SetHeadMoved(42, "moved-head")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n := fake.Enqueued(); len(n) != 0 {
		t.Fatalf("a moved head with CHANGED content must NOT merge, got enqueued=%v", n)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff (real content change -> human gate)", j.State)
	}
}

// TestHeadModifiedCosmeticMoveMerges: russ #214. The PR head moved after the verdict (a
// reviewer's empty --allow-empty findings-commit) but the base..head DIFF is byte-identical
// to what was reviewed — a cosmetic move. The content-equality recovery merges the live
// head instead of rotting in merge_handoff for hours. Safe: it merges only identical content.
func TestHeadModifiedCosmeticMoveMerges(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	// live head "cosmetic-head" — DiffBetween returns the SAME diffOut for every head, so the
	// reviewed and live diffs are identical (an empty commit changes the SHA, not the diff).
	sender.WithHistory(&fakeHistory{tip: "cosmetic-head", diffOut: diffAdding("docs/operating.md", "clean")}, "main")
	mergingJob(t, st, "j")
	fake.SetHeadMoved(42, "cosmetic-head")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n := fake.Enqueued(); len(n) != 1 || n[0] != 42 {
		t.Fatalf("a cosmetic head move with IDENTICAL content must merge, got enqueued=%v", n)
	}
	if got := fake.MergeHead(42); got != "cosmetic-head" {
		t.Fatalf("must re-merge pinned to the content-verified LIVE head, got %q", got)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State == job.StateMergeHandoff {
		t.Fatalf("a content-verified cosmetic move must NOT hand off to a human")
	}
}
