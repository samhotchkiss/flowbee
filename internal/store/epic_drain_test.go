package store

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
)

// TestFanOutReviewedEpicsDrain pins the production trigger the epic flow was missing:
// after the barrier review passes (epic_reviewed=1), the drain releases the children
// from backlog into their own spec flows. Without this drain a reviewed epic's issues
// sat in backlog forever (EpicFanOut existed but nothing ever called it). Idempotent.
func TestFanOutReviewedEpicsDrain(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if err := st.SeedEpic(ctx, SeedEpicParams{
		EpicID: "ep", ChatRef: "c", AuthorLens: "product_speccer",
		IssueIDs: []string{"ep-a", "ep-b"}, Now: now,
	}); err != nil {
		t.Fatalf("seed epic: %v", err)
	}

	// before the barrier passes, the drain must NOT release anything (barrier holds).
	if n, err := st.FanOutReviewedEpics(ctx, now); err != nil || n != 0 {
		t.Fatalf("drain before review: n=%d err=%v, want 0/nil (barrier holds)", n, err)
	}
	for _, id := range []string{"ep-a", "ep-b"} {
		j, _ := st.GetJob(ctx, id)
		if j.State != job.StateBacklog {
			t.Fatalf("child %s state=%s, want backlog before fan-out", id, j.State)
		}
	}

	// the barrier passes (mark reviewed as the sign-off does), then the drain releases.
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET epic_reviewed=1 WHERE id='ep'`); err != nil {
		t.Fatal(err)
	}
	n, err := st.FanOutReviewedEpics(ctx, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("drain released %d children, want 2", n)
	}
	for _, id := range []string{"ep-a", "ep-b"} {
		j, _ := st.GetJob(ctx, id)
		if j.State != job.StateSpecAuthoring {
			t.Fatalf("child %s state=%s, want spec_authoring after fan-out", id, j.State)
		}
	}

	// idempotent: a second drain releases nothing (no backlog children remain).
	if n, err := st.FanOutReviewedEpics(ctx, now.Add(2*time.Minute)); err != nil || n != 0 {
		t.Fatalf("second drain: n=%d err=%v, want 0/nil (idempotent)", n, err)
	}
}
