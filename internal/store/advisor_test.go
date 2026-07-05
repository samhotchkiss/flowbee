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

// TestAdvisorFirstResponderForFailures: the repeated-failure reasons (bounces / attempts /
// reviewer_rejections) go STRAIGHT to the advisor — no mechanical unblock_attempts needed —
// so the system's first response to "build keeps failing review" is a guided correction,
// not a park. And the guided retry earns a FRESH attempts+bounces budget (folded), because a
// blind rebuild with the budget already spent would immediately re-exhaust.
func TestAdvisorFirstResponderForFailures(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	// drive a build to a REAL needs_human(attempts) through penalty releases (consistent
	// ledger): 4 warm-up cycles leave attempts=4, the 5th release exhausts max_attempts=5.
	ep := claimedBuildAt(t, st, "j", 4, now)
	if err := st.Release(ctx, store.ReleaseParams{JobID: "j", Epoch: ep, Now: now}); err != nil {
		t.Fatalf("exhausting release: %v", err)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateNeedsHuman || j.Attempts != 5 {
		t.Fatalf("setup: want needs_human/attempts=5, got %s/%d", j.State, j.Attempts)
	}

	// advisor is the FIRST responder for `attempts` — no mechanical stage to wait on.
	cands, err := st.AdvisorCandidates(ctx, 2, 3)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(cands) != 1 || cands[0].JobID != "j" || cands[0].Reason != string(job.EscalationAttempts) {
		t.Fatalf("want [j/attempts] as an advisor candidate, got %+v", cands)
	}

	rearmed, err := st.ApplyAdvisorVerdict(ctx, "j", "CORRECTION", "the arch-lint failed; run it locally and fix the import cycle", cands[0].TriggerHash, now, 10*time.Minute)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !rearmed {
		t.Fatal("CORRECTION must re-arm")
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateReady {
		t.Fatalf("state=%s, want ready", j.State)
	}
	if j.Attempts != 0 {
		t.Fatalf("attempts=%d, want 0 (advisor-guided retry earns a fresh budget)", j.Attempts)
	}
	if j.StuckHint == "" {
		t.Fatal("advisor correction must be carried as stuck_hint for the guided rebuild")
	}
	// the reset must FOLD — a DR rebuild has to reproduce the fresh budget.
	assertFoldMatchesProjection(t, st, "j")
}

// mergeHandoffWithReason parks a build job in merge_handoff carrying a routing reason (via
// the real RouteSelfMergeToHandoff event), then ages it so the fixer's min-age gate passes.
func mergeHandoffWithReason(t *testing.T, st *store.Store, id, reason string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "b0", RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='mergeable', head_sha='h1' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.RouteSelfMergeToHandoff(ctx, id, reason, now); err != nil {
		t.Fatalf("route handoff %s: %v", id, err)
	}
	// age it in the REAL SQLite datetime('now') format (space-separated, NOT RFC3339) so the
	// fixer's parse is exercised the way production stores timestamps.
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET updated_at='2020-01-01 00:00:00' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
}

// TestMergeFixerEscalatesFixable: a PR parked in merge_handoff for a FIXABLE reason (head
// moved after review) is re-armed to a fixer worker with a "make it mergeable" brief; a
// POLICY denial (denylist/source) stays a human gate; a capped one stops looping.
func TestMergeFixerEscalatesFixable(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(2_000_000_000, 0) // 2033, well past the aged updated_at

	mergeHandoffWithReason(t, st, "fixable", "head_modified_after_review", now)
	mergeHandoffWithReason(t, st, "policy", "denylist:flowbee_source", now)
	mergeHandoffWithReason(t, st, "capped", "head_modified_after_review", now)
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET unblock_attempts=3 WHERE id='capped'`); err != nil {
		t.Fatal(err)
	}

	rep, err := st.EscalateStuckMergeHandoff(ctx, 0, 3, now)
	if err != nil {
		t.Fatalf("merge-fixer: %v", err)
	}
	if len(rep.Escalated) != 1 || rep.Escalated[0] != "fixable" {
		t.Fatalf("Escalated=%v, want [fixable]", rep.Escalated)
	}
	fx, _ := st.GetJob(ctx, "fixable")
	if fx.State != job.StateReady {
		t.Fatalf("fixable state=%s, want ready (re-armed to a fixer)", fx.State)
	}
	if fx.StuckHint == "" || fx.Role != job.RoleEngWorker {
		t.Fatalf("fixable must carry the make-it-mergeable brief as an eng_worker; hint=%q role=%s", fx.StuckHint, fx.Role)
	}
	if p, _ := st.GetJob(ctx, "policy"); p.State != job.StateMergeHandoff {
		t.Fatalf("policy state=%s, want merge_handoff (source denial stays a human gate)", p.State)
	}
	// the capped fixable handoff — the fixer rebuilt it `cap` times and it still won't merge —
	// is auto-cancelled (a genuinely un-landable change), NOT left to loop or park forever.
	if len(rep.Cancelled) != 1 || rep.Cancelled[0] != "capped" {
		t.Fatalf("Cancelled=%v, want [capped]", rep.Cancelled)
	}
	if c, _ := st.GetJob(ctx, "capped"); c.State != job.StateCancelled {
		t.Fatalf("capped state=%s, want cancelled (fixer exhausted — terminal, not a forever-park)", c.State)
	}
}

// TestAutoCancelExhausted: the terminal backstop cancels a job the advisor has exhausted
// (consulted its cap of times, still parked for an advisable reason) so the board
// self-clears — but never a job still under the cap, nor a non-advisable park.
func TestAutoCancelExhausted(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	seedNeedsHuman(t, st, "exhausted", string(job.EscalationBounces), now)
	seedNeedsHuman(t, st, "still-trying", string(job.EscalationBounces), now)
	seedNeedsHuman(t, st, "semantic", string(job.EscalationProjectOut), now)
	// advisor_attempts is projection-only bookkeeping; set it directly for the gate test.
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET advisor_attempts=3 WHERE id IN ('exhausted','semantic')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET advisor_attempts=2 WHERE id='still-trying'`); err != nil {
		t.Fatal(err)
	}

	// maxParkedAge=0 disables the time trigger, isolating the advisor-cap trigger.
	rep, err := st.AutoCancelExhausted(ctx, 3, 0, now)
	if err != nil {
		t.Fatalf("auto-cancel: %v", err)
	}
	if len(rep.Cancelled) != 1 || rep.Cancelled[0] != "exhausted" {
		t.Fatalf("Cancelled=%v, want [exhausted]", rep.Cancelled)
	}
	if j, _ := st.GetJob(ctx, "exhausted"); j.State != job.StateCancelled {
		t.Fatalf("exhausted state=%s, want cancelled (board self-clears)", j.State)
	}
	if j, _ := st.GetJob(ctx, "still-trying"); j.State != job.StateNeedsHuman {
		t.Fatalf("still-trying state=%s, want needs_human (under the cap — keep trying)", j.State)
	}
	if j, _ := st.GetJob(ctx, "semantic"); j.State != job.StateNeedsHuman {
		t.Fatalf("semantic state=%s, want needs_human (project_out needs external action, never auto-cancel)", j.State)
	}
}

// TestAutoCancelFoldConsistent: auto-cancel must fold to exactly the projection — the
// cancelled row clears escalation_reason/over_budget just as the Fold's terminal post-step
// does, so a DR rebuild reproduces it.
func TestAutoCancelFoldConsistent(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(2_000_000_000, 0)

	// real needs_human(attempts) ledger.
	ep := claimedBuildAt(t, st, "j", 4, now)
	if err := st.Release(ctx, store.ReleaseParams{JobID: "j", Epoch: ep, Now: now}); err != nil {
		t.Fatalf("exhaust: %v", err)
	}
	// advisor_attempts is projection-only bookkeeping (not folded); set it to trip the cap.
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET advisor_attempts=3 WHERE id='j'`); err != nil {
		t.Fatal(err)
	}
	rep, err := st.AutoCancelExhausted(ctx, 3, 0, now)
	if err != nil || len(rep.Cancelled) != 1 {
		t.Fatalf("auto-cancel rep=%+v err=%v", rep, err)
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateCancelled || j.EscalationReason != "" {
		t.Fatalf("want cancelled with cleared reason, got %s/%q", j.State, j.EscalationReason)
	}
	assertFoldMatchesProjection(t, st, "j")
}

// TestAdvisorEngagesBlankBounceExhaustion is the regression for a gap found running the
// autonomy preview against live data: a gate bounce-exhaustion routes to needs_human WITHOUT
// stamping escalation_reason (the column is blank), so the advisor — which keyed on the
// column — silently missed it, and a blank reason has no exit → it would park forever. The
// ladder now derives the EFFECTIVE reason from the counters (classifyEscalation), so a
// blank-column job whose bounces are exhausted is correctly engaged as `bounces`.
func TestAdvisorEngagesBlankBounceExhaustion(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(2_000_000_000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "blank", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "b0", RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// needs_human with a BLANK escalation_reason but the bounce budget spent (bounces>=max).
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='needs_human', escalation_reason='', bounces=max_bounces WHERE id='blank'`); err != nil {
		t.Fatal(err)
	}

	cands, err := st.AdvisorCandidates(ctx, 2, 3)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(cands) != 1 || cands[0].JobID != "blank" || cands[0].Reason != string(job.EscalationBounces) {
		t.Fatalf("blank-column bounce-exhaustion must be engaged as bounces, got %+v", cands)
	}
	// and it must have a terminal exit (auto-cancel by time), not park forever.
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET updated_at='2020-01-01 00:00:00' WHERE id='blank'`); err != nil {
		t.Fatal(err)
	}
	rep, err := st.AutoCancelExhausted(ctx, 3, 24*time.Hour, now)
	if err != nil {
		t.Fatalf("auto-cancel: %v", err)
	}
	if len(rep.Cancelled) != 1 || rep.Cancelled[0] != "blank" {
		t.Fatalf("blank bounce-exhaustion must be time-cancelable, got %v", rep.Cancelled)
	}
}

// TestAutonomyPreview: the read-only shadow snapshot lists what the janitor and advisor
// would engage, without mutating anything.
func TestAutonomyPreview(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(2_000_000_000, 0)
	seedWorker(t, st, "live", now)

	seedNeedsHuman(t, st, "rej", string(job.EscalationReviewerRejections), now)
	seedNeedsHuman(t, st, "att", string(job.EscalationAttempts), now)
	seedNeedsHuman(t, st, "gone", string(job.EscalationProjectOut), now) // not advisable

	pv, err := st.AutonomyPreview(ctx, now, time.Hour, 2, 3, 2)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if pv.LiveWorkers != 1 {
		t.Fatalf("LiveWorkers=%d, want 1", pv.LiveWorkers)
	}
	engaged := map[string]string{}
	for _, e := range pv.AdvisorEngage {
		engaged[e.JobID] = e.Reason
	}
	if len(engaged) != 2 || engaged["rej"] == "" || engaged["att"] == "" {
		t.Fatalf("advisor would engage %v, want {rej,att}", pv.AdvisorEngage)
	}
	if _, ok := engaged["gone"]; ok {
		t.Fatal("project_out must NOT be engaged (not advisable)")
	}
	// preview must be READ-ONLY: nothing moved.
	for _, id := range []string{"rej", "att", "gone"} {
		if j, _ := st.GetJob(ctx, id); j.State != job.StateNeedsHuman {
			t.Fatalf("%s moved to %s — preview must not mutate", id, j.State)
		}
	}
}

// TestNoEligibleWorkerConverges: a no_eligible_worker park (a capability dead-end the
// advisor can't fix) must still self-clear via the TIME backstop — not park forever — while
// a genuinely-external park (project_out) is never auto-cancelled.
func TestNoEligibleWorkerConverges(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(2_000_000_000, 0)

	seedNeedsHuman(t, st, "no-worker", string(job.EscalationNoEligibleWorker), now)
	seedNeedsHuman(t, st, "fresh-no-worker", string(job.EscalationNoEligibleWorker), now)
	seedNeedsHuman(t, st, "project-out", string(job.EscalationProjectOut), now)
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET updated_at='2020-01-01 00:00:00' WHERE id IN ('no-worker','project-out')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET updated_at=? WHERE id='fresh-no-worker'`, now.UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}

	rep, err := st.AutoCancelExhausted(ctx, 3, 24*time.Hour, now)
	if err != nil {
		t.Fatalf("auto-cancel: %v", err)
	}
	if len(rep.Cancelled) != 1 || rep.Cancelled[0] != "no-worker" {
		t.Fatalf("Cancelled=%v, want [no-worker] (time backstop clears the dead-end)", rep.Cancelled)
	}
	if j, _ := st.GetJob(ctx, "fresh-no-worker"); j.State != job.StateNeedsHuman {
		t.Fatalf("fresh-no-worker state=%s, want needs_human (not yet aged)", j.State)
	}
	if j, _ := st.GetJob(ctx, "project-out"); j.State != job.StateNeedsHuman {
		t.Fatalf("project-out state=%s, want needs_human (external action — never auto-cancel)", j.State)
	}
}

// TestAutoCancelTimeBackstop: a job stuck BELOW the advisor cap (e.g. the advisor deduped
// out because the re-armed build produced no new head) must still clear via the time
// backstop — nothing parks forever. A recently-touched park is NOT cancelled.
func TestAutoCancelTimeBackstop(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(2_000_000_000, 0)

	seedNeedsHuman(t, st, "aged", string(job.EscalationBounces), now)
	seedNeedsHuman(t, st, "fresh", string(job.EscalationBounces), now)
	// both under the advisor cap; only "aged" is past maxParkedAge. Use the REAL SQLite
	// datetime('now') format (space-separated) — an RFC3339 literal here would let the parse
	// bug hide, which is exactly how this shipped inert once.
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET advisor_attempts=1, updated_at='2020-01-01 00:00:00' WHERE id='aged'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET advisor_attempts=1, updated_at=? WHERE id='fresh'`, now.UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}

	rep, err := st.AutoCancelExhausted(ctx, 3, 24*time.Hour, now)
	if err != nil {
		t.Fatalf("auto-cancel: %v", err)
	}
	if len(rep.Cancelled) != 1 || rep.Cancelled[0] != "aged" {
		t.Fatalf("Cancelled=%v, want [aged] (time backstop)", rep.Cancelled)
	}
	if j, _ := st.GetJob(ctx, "fresh"); j.State != job.StateNeedsHuman {
		t.Fatalf("fresh state=%s, want needs_human (recently active — keep trying)", j.State)
	}
}

// TestLadderConverges is the composition test: a job that KEEPS failing must climb the whole
// ladder and TERMINATE — never park forever. It simulates the real loop: the advisor engages
// while under its cap (each cycle a fresh head, so dedup doesn't block), and once the cap is
// spent the terminal backstop cancels it. This is the "the board self-clears" guarantee
// exercised end-to-end at the store layer.
func TestLadderConverges(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(2_000_000_000, 0)
	const advisorCap = 3

	// a real needs_human(attempts) job (consistent ledger).
	ep := claimedBuildAt(t, st, "j", 4, now)
	if err := st.Release(ctx, store.ReleaseParams{JobID: "j", Epoch: ep, Now: now}); err != nil {
		t.Fatalf("exhaust: %v", err)
	}

	// the advisor engages exactly advisorCap times, each on a distinct head (a real rebuild
	// would produce a new commit), then is capped out.
	for i := 0; i < advisorCap; i++ {
		cands, err := st.AdvisorCandidates(ctx, 2, advisorCap)
		if err != nil {
			t.Fatalf("candidates round %d: %v", i, err)
		}
		if len(cands) != 1 {
			t.Fatalf("round %d: want 1 candidate, got %d (advisor should keep engaging under the cap)", i, len(cands))
		}
		rearmed, err := st.ApplyAdvisorVerdict(ctx, "j", "CORRECTION", "try approach", cands[0].TriggerHash, now, 0)
		if err != nil || !rearmed {
			t.Fatalf("round %d: apply rearmed=%v err=%v", i, rearmed, err)
		}
		// simulate the guided build failing again: back to needs_human(attempts) at a NEW head.
		if _, err := st.DB.ExecContext(ctx,
			`UPDATE jobs SET state='needs_human', escalation_reason='attempts', head_sha=? WHERE id='j'`,
			"h"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	// capped out: the advisor no longer engages.
	if cands, _ := st.AdvisorCandidates(ctx, 2, advisorCap); len(cands) != 0 {
		t.Fatalf("after %d consults the advisor must be capped, still got %d candidates", advisorCap, len(cands))
	}
	// terminal backstop closes it out — the board self-clears.
	rep, err := st.AutoCancelExhausted(ctx, advisorCap, 24*time.Hour, now)
	if err != nil {
		t.Fatalf("auto-cancel: %v", err)
	}
	if len(rep.Cancelled) != 1 || rep.Cancelled[0] != "j" {
		t.Fatalf("Cancelled=%v, want [j] (ladder must terminate)", rep.Cancelled)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateCancelled {
		t.Fatalf("final state=%s, want cancelled — the ladder converged", j.State)
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
