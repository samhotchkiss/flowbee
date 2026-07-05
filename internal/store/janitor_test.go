package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// janitorLiveCfg drives a job to needs_human(stall) in a SINGLE two-rung kill:
// GovernorCeiling=1 means the first stall revocation trips the anti-thrash ceiling and
// escalates instead of re-dispatching. A small PhaseBudget makes the soft deadline cross
// quickly; the corroborating second rung is Rung-2 (SHA stalled past the window).
var janitorLiveCfg = store.LivenessConfig{
	PhaseBudget: time.Minute, AbsoluteCap: 60 * time.Minute,
	Rung2Window: 10 * time.Minute, GovernorCeiling: 1, CircuitBreakerAbstainFraction: 0.9,
}

// escalateStall drives a fresh build job all the way to needs_human with escalation
// reason `stall` through the REAL liveness ladder (not a raw UPDATE), so the ledger folds
// consistently and the janitor sees exactly what production produces.
func escalateStall(t *testing.T, st *store.Store, id string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "b0", RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ls, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: id, LeaseID: "lease-" + id, Identity: "builder", ModelFamily: "codex",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker", "model_family:codex"},
		TTL: time.Hour, Now: now,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.ArmLeaseLivenessTimers(ctx, id, ls.Epoch, now, janitorLiveCfg); err != nil {
		t.Fatalf("arm timers: %v", err)
	}
	// a reconciled head opens the Rung-2 window (converging), then the same SHA past the
	// window is `stalled` — the corroborating rung for the soft-deadline kill.
	if err := st.UpsertDomainBFacts(ctx, id, job.DomainBFacts{
		PRExists: true, PRNumber: 1, HeadSHA: "h1", BaseSHA: "b0",
	}); err != nil {
		t.Fatalf("facts: %v", err)
	}
	if _, err := st.Rung2Sweep(ctx, store.DBFactSource{DB: st.DB}, now, janitorLiveCfg); err != nil {
		t.Fatalf("sweep1: %v", err)
	}
	later := now.Add(11 * time.Minute)
	if _, err := st.Rung2Sweep(ctx, store.DBFactSource{DB: st.DB}, later, janitorLiveCfg); err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	res, err := st.EvaluateLiveness(ctx, id, later, janitorLiveCfg, false)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !res.Escalated {
		t.Fatalf("setup: expected a stall escalation to needs_human, got killed=%v to=%s reason=%s", res.Killed, res.ToState, res.Reason)
	}
	j, _ := st.GetJob(ctx, id)
	if j.State != job.StateNeedsHuman || j.EscalationReason != string(job.EscalationStall) {
		t.Fatalf("setup: want needs_human/stall, got %s/%s", j.State, j.EscalationReason)
	}
}

// TestJanitorUnblocksStall: the core self-unblock path. A job parked in needs_human for
// the MECHANICAL `stall` reason is automatically re-armed to `ready` by the janitor —
// no operator requeue — with the attempts budget PRESERVED (not reset), the unblock
// counter incremented, and the ledger folding consistently (projection == Fold).
func TestJanitorUnblocksStall(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	tEval := now.Add(11 * time.Minute)

	escalateStall(t, st, "j", now)
	seedWorker(t, st, "live", tEval) // a live worker so the janitor has somewhere to route

	before, _ := st.GetJob(ctx, "j")

	rep, err := st.JanitorUnblock(ctx, tEval, time.Hour, store.JanitorConfig{})
	if err != nil {
		t.Fatalf("janitor: %v", err)
	}
	if rep.Unblocked != 1 {
		t.Fatalf("Unblocked=%d, want 1", rep.Unblocked)
	}

	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateReady {
		t.Fatalf("state=%s, want ready (auto-unblocked)", j.State)
	}
	if j.Role != job.RoleEngWorker {
		t.Fatalf("role=%s, want eng_worker (re-armed as a builder)", j.Role)
	}
	if j.EscalationReason != "" {
		t.Fatalf("escalation_reason=%q, want cleared (no longer parked)", j.EscalationReason)
	}
	if j.UnblockAttempts != 1 {
		t.Fatalf("unblock_attempts=%d, want 1", j.UnblockAttempts)
	}
	if j.HeadSHA != "" {
		t.Fatalf("head_sha=%q, want cleared for a fresh build candidate", j.HeadSHA)
	}
	// attempts is PRESERVED — the janitor is bounded, not a budget reset (unlike operator requeue).
	if j.Attempts != before.Attempts {
		t.Fatalf("attempts=%d, want preserved %d (janitor must not reset the build budget)", j.Attempts, before.Attempts)
	}
	if j.LeaseEpoch <= before.LeaseEpoch {
		t.Fatalf("lease_epoch=%d must be bumped past %d (fence any zombie)", j.LeaseEpoch, before.LeaseEpoch)
	}
	// the whole point: the ledger must fold to exactly this projection (DR-safe).
	assertFoldMatchesProjection(t, st, "j")
}

// TestJanitorRespectsCap: a job that keeps re-stalling must CONVERGE back to needs_human
// rather than loop forever. With MaxUnblockAttempts already spent, the janitor leaves it
// parked for a human.
func TestJanitorRespectsCap(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	// a needs_human(stall) job that has already been auto-unblocked twice.
	seedNeedsHuman(t, st, "j", string(job.EscalationStall), now)
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET unblock_attempts = 2 WHERE id = 'j'`); err != nil {
		t.Fatal(err)
	}
	seedWorker(t, st, "live", now)

	rep, err := st.JanitorUnblock(ctx, now, time.Hour, store.JanitorConfig{MaxUnblockAttempts: 2})
	if err != nil {
		t.Fatalf("janitor: %v", err)
	}
	if rep.Unblocked != 0 {
		t.Fatalf("Unblocked=%d, want 0 (cap reached — leave it parked)", rep.Unblocked)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateNeedsHuman {
		t.Fatalf("state=%s, want needs_human (capped job stays parked)", j.State)
	}
}

// TestJanitorCorrelatedBreaker: when many jobs are parked for the SAME reason, a shared
// root cause is likelier than N independent stalls. The janitor stands down on that
// reason (a fix-once page) instead of fanning N doomed requeues into the same broken cause.
func TestJanitorCorrelatedBreaker(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	seedWorker(t, st, "live", now)

	ids := []string{"a", "b", "c", "d", "e"} // == CorrelatedThreshold default (5)
	for _, id := range ids {
		seedNeedsHuman(t, st, id, string(job.EscalationStall), now)
	}

	rep, err := st.JanitorUnblock(ctx, now, time.Hour, store.JanitorConfig{})
	if err != nil {
		t.Fatalf("janitor: %v", err)
	}
	if rep.Unblocked != 0 {
		t.Fatalf("Unblocked=%d, want 0 (correlated-failure breaker must stand down)", rep.Unblocked)
	}
	if len(rep.StoodDown) != 1 || rep.StoodDown[0] != string(job.EscalationStall) {
		t.Fatalf("StoodDown=%v, want [stall]", rep.StoodDown)
	}
	for _, id := range ids {
		if j, _ := st.GetJob(ctx, id); j.State != job.StateNeedsHuman {
			t.Fatalf("%s state=%s, want needs_human (breaker tripped, none moved)", id, j.State)
		}
	}
}

// TestJanitorIgnoresSemanticReasons: the janitor NEVER auto-requeues a semantic dead-end
// (attempts/project_out/pr_closed/cost/reviewer_rejections/design). Those stay parked for
// a human — a blind retry would just re-fail or fight a human's decision.
func TestJanitorIgnoresSemanticReasons(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	seedWorker(t, st, "live", now)

	for _, reason := range []string{
		string(job.EscalationAttempts), string(job.EscalationProjectOut),
		string(job.EscalationPRClosed), string(job.EscalationCost),
		string(job.EscalationReviewerRejections), string(job.EscalationCIStalled),
	} {
		seedNeedsHuman(t, st, "j-"+reason, reason, now)
	}

	rep, err := st.JanitorUnblock(ctx, now, time.Hour, store.JanitorConfig{})
	if err != nil {
		t.Fatalf("janitor: %v", err)
	}
	if rep.Unblocked != 0 {
		t.Fatalf("Unblocked=%d, want 0 (semantic reasons must stay parked)", rep.Unblocked)
	}
}

// TestJanitorNoLiveFleet: with no live worker, unblocking to `ready` just relocates the
// job to a queue nobody drains. The janitor stands down entirely (ReconcileStuck /
// fleet-health own the down-fleet signal).
func TestJanitorNoLiveFleet(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	seedNeedsHuman(t, st, "j", string(job.EscalationStall), now)
	// NO seedWorker: the fleet is down.

	rep, err := st.JanitorUnblock(ctx, now, time.Hour, store.JanitorConfig{})
	if err != nil {
		t.Fatalf("janitor: %v", err)
	}
	if rep.Unblocked != 0 {
		t.Fatalf("Unblocked=%d, want 0 (no live fleet — nothing to unblock onto)", rep.Unblocked)
	}
}

// seedNeedsHuman parks a build job directly in needs_human with the given reason. This is
// a projection-only shortcut for the DECISION-gate tests (cap/breaker/reason-filter/fleet)
// where the ledger-fold faithfulness is covered separately by TestJanitorUnblocksStall.
func seedNeedsHuman(t *testing.T, st *store.Store, id, reason string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "b0", RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='needs_human', escalation_reason=? WHERE id=?`, reason, id); err != nil {
		t.Fatalf("park %s: %v", id, err)
	}
}
