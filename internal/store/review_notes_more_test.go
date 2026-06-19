package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/spec"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestReviewFindingsKeptOnEmptyBounce: a later bounce with NO notes must KEEP the prior
// findings (the CASE-guarded UPDATE mirrors Fold's "only set on non-empty ReviewNotes"
// guard — else projection != Fold for an empty-notes bounce).
func TestReviewFindingsKeptOnEmptyBounce(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	src := store.DBFactSource{DB: st.DB}
	policy := job.Policy{}

	driveToCodeReview(t, st, "ej", "h0", "b0")
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET max_bounces=20 WHERE id='ej'`); err != nil {
		t.Fatal(err)
	}
	const first = "fix the nil deref in parseConfig"
	if _, err := st.ReviewResult(ctx, src, policy, store.ReviewResultParams{
		JobID: "ej", Epoch: epochOf(t, st, "ej"), Claim: job.VerdictChangesRequested, Notes: first, Now: time.Unix(3000, 0),
	}); err != nil {
		t.Fatalf("first bounce: %v", err)
	}
	reReview(t, st, "ej", "reviewer-ej", 1)
	if _, err := st.ReviewResult(ctx, src, policy, store.ReviewResultParams{
		JobID: "ej", Epoch: epochOf(t, st, "ej"), Claim: job.VerdictChangesRequested, Notes: "", Now: time.Unix(3200, 0),
	}); err != nil {
		t.Fatalf("empty bounce: %v", err)
	}
	if j, _ := st.GetJob(ctx, "ej"); j.LastReviewNotes != first {
		t.Fatalf("empty bounce cleared prior findings: got %q want %q", j.LastReviewNotes, first)
	}
	assertFoldMatchesProjection(t, st, "ej")
}

// TestSpecReviewFindingsCarriedToRebuild: the symmetric spec-review path — an
// issue-review changes_requested carries the reviewer's findings to the spec-author
// rebuild (LastReviewNotes), fold-consistently.
func TestSpecReviewFindingsCarriedToRebuild(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Unix(6000, 0)
	const id = "sj"
	if _, err := st.SeedSpecJob(ctx, store.SeedSpecParams{
		ID: id, ChatRef: "c", AuthorLens: "product_speccer", Now: now,
	}); err != nil {
		t.Fatalf("seed spec: %v", err)
	}
	hash := spec.ContentHash([]byte("the draft spec"))
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='spec_review', stage='review', role='spec_reviewer',
		 spec_content_hash=?, reviewer_lens='engineering_manager' WHERE id=?`, hash, id); err != nil {
		t.Fatalf("setup spec_review: %v", err)
	}
	const findings = "Acceptance criteria are ambiguous; specify the error codes."
	if _, err := st.SpecReviewResult(ctx, store.SpecReviewResultParams{
		JobID: id, Epoch: 0, Claim: job.VerdictChangesRequested, BindsTo: hash, Notes: findings, Now: now,
	}); err != nil {
		t.Fatalf("spec bounce: %v", err)
	}
	j, _ := st.GetJob(ctx, id)
	if j.State != job.StateSpecAuthoring {
		t.Fatalf("state=%s want spec_authoring (bounced to author)", j.State)
	}
	if j.LastReviewNotes != findings {
		t.Fatalf("spec LastReviewNotes=%q want the issue-reviewer findings", j.LastReviewNotes)
	}
	assertFoldMatchesProjection(t, st, id)
}
