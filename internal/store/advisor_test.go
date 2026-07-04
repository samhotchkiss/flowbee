package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// markMechanicallyExhausted raises unblock_attempts so the job is past the mechanical
// janitor's cap — the precondition for the advisor to be consulted.
func markMechanicallyExhausted(t *testing.T, st *store.Store, id string, n int) {
	t.Helper()
	if _, err := st.DB.ExecContext(context.Background(),
		`UPDATE jobs SET unblock_attempts=? WHERE id=?`, n, id); err != nil {
		t.Fatal(err)
	}
}

// TestAdvisorCandidatesGating: only stalls the mechanical janitor gave up on, under the
// advisor cap, and not already consulted at this signature are eligible.
func TestAdvisorCandidatesGating(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	seedNeedsHuman(t, st, "fresh-stall", string(job.EscalationStall), now) // unblock_attempts=0 -> not yet advisor's
	seedNeedsHuman(t, st, "exhausted", string(job.EscalationStall), now)
	markMechanicallyExhausted(t, st, "exhausted", 2)
	seedNeedsHuman(t, st, "semantic", string(job.EscalationProjectOut), now) // never advisable
	markMechanicallyExhausted(t, st, "semantic", 2)

	cands, err := st.AdvisorCandidates(ctx, 2, 3)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(cands) != 1 || cands[0].JobID != "exhausted" {
		t.Fatalf("candidates=%+v, want just [exhausted]", cands)
	}
	if cands[0].TriggerHash == "" {
		t.Fatal("expected a trigger hash for dedup")
	}
}

// TestAdvisorStopLeavesParked: a STOP verdict records the consult (so the model isn't
// re-run at the same signature) but leaves the job in needs_human.
func TestAdvisorStopLeavesParked(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	seedNeedsHuman(t, st, "j", string(job.EscalationStall), now)
	markMechanicallyExhausted(t, st, "j", 2)

	cands, _ := st.AdvisorCandidates(ctx, 2, 3)
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(cands))
	}
	rearmed, err := st.ApplyAdvisorVerdict(ctx, "j", "STOP", "needs a human", cands[0].TriggerHash, now, 10*time.Minute)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rearmed {
		t.Fatal("STOP must not re-arm")
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateNeedsHuman {
		t.Fatalf("state=%s, want needs_human (STOP leaves it parked)", j.State)
	}
	// dedup: same signature is no longer a candidate (advisor_last_hash recorded).
	if again, _ := st.AdvisorCandidates(ctx, 2, 3); len(again) != 0 {
		t.Fatalf("consulted signature must be deduped, still got %d candidates", len(again))
	}
}

// TestAdvisorRearmsWithHintFoldConsistent: a PLAN verdict re-arms the job to ready with the
// note carried as stuck_hint, and the ledger folds to exactly the projection (DR-safe). The
// needs_human(stall) state is built through the REAL liveness ladder so the ledger is
// consistent for the fold-invariant assertion.
func TestAdvisorRearmsWithHintFoldConsistent(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	escalateStall(t, st, "j", now) // real needs_human(stall) ledger
	markMechanicallyExhausted(t, st, "j", 2)
	tEval := now.Add(20 * time.Minute)

	cands, _ := st.AdvisorCandidates(ctx, 2, 3)
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(cands))
	}
	rearmed, err := st.ApplyAdvisorVerdict(ctx, "j", "PLAN", "decompose into A then B", cands[0].TriggerHash, tEval, 10*time.Minute)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !rearmed {
		t.Fatal("PLAN must re-arm the job")
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateReady {
		t.Fatalf("state=%s, want ready", j.State)
	}
	if j.StuckHint != "decompose into A then B" {
		t.Fatalf("stuck_hint=%q, want the advisor note (for lease-context re-entry)", j.StuckHint)
	}
	if j.EscalationReason != "" {
		t.Fatalf("escalation_reason=%q, want cleared", j.EscalationReason)
	}
	// the ledger must fold to exactly this projection — the whole point of event-sourcing.
	assertFoldMatchesProjection(t, st, "j")

	// advisor cap: after advisor_attempts reaches the cap, the job is no longer a candidate.
	markMechanicallyExhausted(t, st, "j", 2) // (re-arm reset it; restore for the gate check)
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='needs_human', escalation_reason='stall', advisor_attempts=3 WHERE id='j'`); err != nil {
		t.Fatal(err)
	}
	if capped, _ := st.AdvisorCandidates(ctx, 2, 3); len(capped) != 0 {
		t.Fatalf("advisor cap must exclude the job, got %d candidates", len(capped))
	}
}
