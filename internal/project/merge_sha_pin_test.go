package project

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
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

func TestAutonomousMergeHeadMoveRearmsReviewAndAbandonsOutbox(t *testing.T) {
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
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateReady || j.Verdict != nil {
		t.Fatalf("state/verdict=%s/%+v, want ready/nil after stale merge authorization", j.State, j.Verdict)
	}
	var status string
	if err := st.DB.QueryRowContext(ctx,
		`SELECT status FROM outbox WHERE job_id='j' AND action=?`, store.ActionEnqueueMerge).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "abandoned" {
		t.Fatalf("stale merge outbox status=%q want abandoned", status)
	}
}

// Byte-identical content is still a different commit. Independent review and CI
// authorize an exact SHA, not a tree equivalence class.
func TestHeadModifiedCosmeticMoveAlsoRearmsReview(t *testing.T) {
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
	if n := fake.Enqueued(); len(n) != 0 {
		t.Fatalf("a different live SHA must not merge even with identical content, got %v", n)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateReady || j.Verdict != nil {
		t.Fatalf("cosmetic SHA move state/verdict=%s/%+v want ready/nil", j.State, j.Verdict)
	}
}
