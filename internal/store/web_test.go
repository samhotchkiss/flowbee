package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestJobStageTimingsFold proves the F12 detail-drawer read-model: the per-stage
// ENTERED/LEFT absolute times are folded from the ledger's state transitions. A
// claimed build job records ready -> (lease_claimed) leased; the ready span is
// closed at the claim instant and the leased span is left open (asOf bounds it).
func TestJobStageTimingsFold(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seeded := time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)
	claimed := seeded.Add(5 * time.Minute)
	asOf := claimed.Add(3 * time.Minute)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, TaskText: "do a thing", Now: seeded,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "j1", LeaseID: "L", Identity: "w", ModelFamily: "claude",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker"},
		TTL: time.Hour, Now: claimed,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	timings, err := st.JobStageTimings(ctx, "j1", asOf)
	if err != nil {
		t.Fatalf("timings: %v", err)
	}
	if len(timings) < 2 {
		t.Fatalf("want >=2 stage spans, got %d: %+v", len(timings), timings)
	}
	// the first span is `ready` (entered at seed, left at claim).
	ready := timings[0]
	if ready.Stage != string(job.StateReady) {
		t.Fatalf("first span stage = %q, want ready", ready.Stage)
	}
	if !ready.Entered.Equal(seeded) {
		t.Fatalf("ready entered = %v, want %v", ready.Entered, seeded)
	}
	if !ready.Left.Equal(claimed) {
		t.Fatalf("ready left = %v, want %v (claim instant)", ready.Left, claimed)
	}
	if ready.Open {
		t.Fatalf("ready span must be closed once the job left it")
	}
	if ready.DurationS != 300 {
		t.Fatalf("ready duration = %ds, want 300", ready.DurationS)
	}
	// the final span is `leased`, still open, bounded by asOf.
	last := timings[len(timings)-1]
	if last.Stage != string(job.StateLeased) {
		t.Fatalf("last span = %q, want leased", last.Stage)
	}
	if !last.Open {
		t.Fatalf("the current stage span must be open")
	}
	if last.DurationS != 180 {
		t.Fatalf("open span duration = %ds, want 180 (asOf - entered)", last.DurationS)
	}
}

// TestBoardCardsStageTimerDeterministic proves the board card's stage-age (the
// gray->amber->red per-card timer source) is folded from the ledger's last
// state-entry event — deterministic, independent of the wall-clock updated_at.
func TestBoardCardsStageTimerDeterministic(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	entered := time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC)
	now := entered.Add(40 * time.Minute)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "card1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, TaskText: "headline\nbody", Now: entered,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cards, err := st.BoardCards(ctx, now)
	if err != nil {
		t.Fatalf("board cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("want 1 card, got %d", len(cards))
	}
	c := cards[0]
	if c.Title != "headline" {
		t.Fatalf("title = %q, want first task line 'headline'", c.Title)
	}
	if c.StageAgeS != 2400 {
		t.Fatalf("stage age = %ds, want 2400 (40m folded from the ready event)", c.StageAgeS)
	}
}

func TestBoardCardsExposeCIState(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "review-1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, TaskText: "review me", Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs SET state='review_pending', stage='review', pr_number=42 WHERE id='review-1'`); err != nil {
		t.Fatalf("move to review_pending: %v", err)
	}
	if err := st.UpsertDomainBFacts(ctx, "review-1", job.DomainBFacts{
		PRExists: true, PRNumber: 42, HeadSHA: "head", BaseSHA: "base", CIGreen: true,
	}); err != nil {
		t.Fatalf("facts: %v", err)
	}

	cards, err := st.BoardCards(ctx, now)
	if err != nil {
		t.Fatalf("board cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("want 1 card, got %d", len(cards))
	}
	if cards[0].CILabel != "CI green" || cards[0].CIClass != "green" {
		t.Fatalf("ci chip = %q/%q, want CI green/green", cards[0].CILabel, cards[0].CIClass)
	}
}
