package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/spec"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestEpicBarrierGatesFanOut proves the F4 epic-level review barrier: an epic's
// child issues sit in backlog until the epic-level review passes, then fan out
// exactly once into the per-issue spec flow. Fan-out before the review is refused.
func TestEpicBarrierGatesFanOut(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(2000, 0)

	epic := "ep-1"
	kids := []string{"ep-1-a", "ep-1-b"}
	if err := st.SeedEpic(ctx, store.SeedEpicParams{
		EpicID: epic, ChatRef: "c", AuthorLens: "product_speccer", IssueIDs: kids, Now: now,
	}); err != nil {
		t.Fatalf("seed epic: %v", err)
	}

	// children start in backlog (tracked, NOT scheduled).
	for _, k := range kids {
		j, err := st.GetJob(ctx, k)
		if err != nil {
			t.Fatalf("get child: %v", err)
		}
		if j.State != job.StateBacklog || j.EpicID != epic || j.IsEpic {
			t.Fatalf("child %s: state=%s epic=%s isEpic=%v want backlog under %s", k, j.State, j.EpicID, j.IsEpic, epic)
		}
	}
	children, err := st.EpicChildren(ctx, epic)
	if err != nil || len(children) != 2 {
		t.Fatalf("EpicChildren got %v err=%v", children, err)
	}

	// the barrier holds: fan-out before the review is refused.
	if _, err := st.EpicFanOut(ctx, epic, now); err == nil {
		t.Fatal("fan-out before the epic review must be refused")
	}
	if r, _ := st.EpicReviewed(ctx, epic); r {
		t.Fatal("epic must not be reviewed before the gate passes")
	}

	// pass the epic-level review (the barrier job is a spec_review at hash "").
	resp, err := st.SpecReviewResult(ctx, store.SpecReviewResultParams{
		JobID: epic, Epoch: 0, Claim: job.VerdictSignedOff, BindsTo: "",
		MeetsStyle: true, MeetsRequirements: true, Now: now,
	})
	if err != nil {
		t.Fatalf("epic review: %v", err)
	}
	if !resp.Minted || resp.JobState != string(job.StateDone) {
		t.Fatalf("epic review must pass the barrier, got %+v", resp)
	}
	if r, _ := st.EpicReviewed(ctx, epic); !r {
		t.Fatal("epic must be reviewed after the gate passes")
	}
	// the barrier records an epic_reviewed event and materializes NO single issue.
	evs, _ := st.LoadEvents(ctx, epic)
	sawEpic, sawIssue := false, false
	for _, e := range evs {
		switch e.Kind {
		case ledger.KindEpicReviewed:
			sawEpic = true
		case ledger.KindIssueMaterialized:
			sawIssue = true
		}
	}
	if !sawEpic || sawIssue {
		t.Fatalf("epic barrier: sawEpicReviewed=%v sawIssueMaterialized=%v (want true,false)", sawEpic, sawIssue)
	}

	// NOW fan out: every child moves backlog -> spec_authoring, exactly once.
	released, err := st.EpicFanOut(ctx, epic, now)
	if err != nil {
		t.Fatalf("fan out: %v", err)
	}
	if len(released) != 2 {
		t.Fatalf("fan-out must release every child once, got %v", released)
	}
	for _, k := range kids {
		if j, _ := st.GetJob(ctx, k); j.State != job.StateSpecAuthoring {
			t.Fatalf("fanned-out child %s must enter spec_authoring, got %s", k, j.State)
		}
	}
	if again, _ := st.EpicFanOut(ctx, epic, now); len(again) != 0 {
		t.Fatalf("re-fan-out must be idempotent, got %v", again)
	}
}

// TestNeedsDesignAndResolve proves the F4 design-fork escalation + resume at the
// store layer: a needs_design verdict parks the job, surfaces it on NeedsInput, and
// ResolveDesign re-arms the spec_review gate.
func TestNeedsDesignAndResolve(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(3000, 0)

	id := "sp-design"
	if _, err := st.SeedSpecJob(ctx, store.SeedSpecParams{
		ID: id, ChatRef: "c", AuthorLens: "product_speccer", Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// drive the job into spec_review with a known hash (skip the lease dance).
	hash := spec.ContentHash([]byte("needs a product decision"))
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='spec_review', stage='review', role='spec_reviewer',
		 spec_content_hash=?, reviewer_lens='engineering_manager' WHERE id=?`, hash, id); err != nil {
		t.Fatalf("setup: %v", err)
	}

	resp, err := st.SpecReviewResult(ctx, store.SpecReviewResultParams{
		JobID: id, Epoch: 0, Claim: job.VerdictNeedsDesign, BindsTo: hash, Now: now,
	})
	if err != nil {
		t.Fatalf("needs_design: %v", err)
	}
	if !resp.NeedsDesign || resp.Minted || resp.JobState != string(job.StateNeedsDesign) {
		t.Fatalf("a design fork must park in needs_design, got %+v", resp)
	}

	j, _ := st.GetJob(ctx, id)
	if j.State != job.StateNeedsDesign || j.EscalationReason != string(job.EscalationDesign) {
		t.Fatalf("needs_design projection: state=%s reason=%s", j.State, j.EscalationReason)
	}

	items, err := st.NeedsInput(ctx)
	if err != nil || len(items) != 1 || items[0].JobID != id {
		t.Fatalf("NeedsInput must surface the design fork, got %+v err=%v", items, err)
	}

	// resolve: re-arm spec_review, clear the reason, leave the needs-input surface.
	if err := st.ResolveDesign(ctx, id, "", 0, now); err != nil {
		t.Fatalf("resolve design: %v", err)
	}
	j, _ = st.GetJob(ctx, id)
	if j.State != job.StateSpecReview || j.EscalationReason != "" {
		t.Fatalf("resolve must re-arm spec_review + clear reason, got state=%s reason=%s", j.State, j.EscalationReason)
	}
	if items, _ := st.NeedsInput(ctx); len(items) != 0 {
		t.Fatalf("a resolved job must leave NeedsInput, got %+v", items)
	}

	// determinism: Fold(events) reproduces the resolved projection.
	evs, _ := st.LoadEvents(ctx, id)
	folded, _ := ledger.Fold(evs)
	if folded.State != job.StateSpecReview {
		t.Fatalf("Fold != projection after resolve: %s", folded.State)
	}
}
