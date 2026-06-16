package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/liveness"
)

func newLiveStore(t *testing.T) *Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "live.db")
	st, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := MigrateUp(context.Background(), st.DB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedLeased seeds a build job and drives it to `leased` (epoch 1) with the liveness
// timers armed, returning the bound epoch.
func seedLeased(t *testing.T, st *Store, jobID string, now time.Time, cfg LivenessConfig) int {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "b0", Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ls, err := st.ClaimReadyJob(ctx, ClaimParams{
		JobID: jobID, LeaseID: "lease-" + jobID, Identity: "w", ModelFamily: "codex",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker", "model_family:codex"},
		TTL: time.Hour, Now: now,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.ArmLeaseLivenessTimers(ctx, jobID, ls.Epoch, now, cfg); err != nil {
		t.Fatalf("arm: %v", err)
	}
	return ls.Epoch
}

var liveCfg = LivenessConfig{
	PhaseBudget: 10 * time.Minute, AbsoluteCap: 60 * time.Minute,
	Rung2Window: 10 * time.Minute, GovernorCeiling: 2, CircuitBreakerAbstainFraction: 0.9,
}

// TestRung2_AbstainsWithoutSHA: a build job before its first ref push (no reconciled
// head SHA) -> Rung-2 abstains (blind, §10.2).
func TestRung2_AbstainsWithoutSHA(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	seedLeased(t, st, "j1", now, liveCfg)

	tripped, err := st.Rung2Sweep(ctx, DBFactSource{DB: st.DB}, now, liveCfg)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if v, _ := st.Rung2VerdictFor(ctx, "j1"); v != liveness.Rung2Abstain {
		t.Fatalf("no SHA must abstain, got %s", v)
	}
	if !tripped {
		t.Fatalf("with the only active job abstaining (1.0 >= 0.9) the breaker must trip")
	}
}

// TestRung2_ConvergingThenStalled: a reconciled head SHA opens the window
// (converging); the SHA unchanged past the window -> stalled.
func TestRung2_ConvergingThenStalled(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	seedLeased(t, st, "j1", now, liveCfg)
	if err := st.UpsertDomainBFacts(ctx, "j1", job.DomainBFacts{
		PRExists: true, PRNumber: 1, HeadSHA: "h1", BaseSHA: "b0",
	}); err != nil {
		t.Fatalf("facts: %v", err)
	}
	if _, err := st.Rung2Sweep(ctx, DBFactSource{DB: st.DB}, now, liveCfg); err != nil {
		t.Fatalf("sweep1: %v", err)
	}
	if v, _ := st.Rung2VerdictFor(ctx, "j1"); v != liveness.Rung2Converging {
		t.Fatalf("first SHA must converge, got %s", v)
	}
	// SHA unchanged, window aged past 10 min -> stalled.
	later := now.Add(11 * time.Minute)
	if _, err := st.Rung2Sweep(ctx, DBFactSource{DB: st.DB}, later, liveCfg); err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	if v, _ := st.Rung2VerdictFor(ctx, "j1"); v != liveness.Rung2Stalled {
		t.Fatalf("stale SHA past the window must stall, got %s", v)
	}
}

// TestRung2_CITransitionExtendsTolerance: with the same SHA but CI running, Rung-2
// stays off `stalled` (Guardrail A, §10.4) — the suite IS progress reaching the
// outside world.
func TestRung2_CITransitionExtendsTolerance(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	seedLeased(t, st, "j1", now, liveCfg)
	if err := st.UpsertDomainBFacts(ctx, "j1", job.DomainBFacts{
		PRExists: true, PRNumber: 1, HeadSHA: "h1", BaseSHA: "b0",
	}); err != nil {
		t.Fatalf("facts: %v", err)
	}
	if _, err := st.Rung2Sweep(ctx, DBFactSource{DB: st.DB}, now, liveCfg); err != nil {
		t.Fatalf("sweep1: %v", err)
	}
	if err := st.MarkCIRunning(ctx, "j1", true, now); err != nil {
		t.Fatalf("mark ci: %v", err)
	}
	// even far past the window, CI-running holds Rung-2 at converging.
	later := now.Add(40 * time.Minute)
	if _, err := st.Rung2Sweep(ctx, DBFactSource{DB: st.DB}, later, liveCfg); err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	if v, _ := st.Rung2VerdictFor(ctx, "j1"); v == liveness.Rung2Stalled {
		t.Fatalf("CI-running must extend tolerance (never stalled), got %s", v)
	}
}

// TestRung2_SpecFlowForcesAbstain: spec-flow jobs have no SHA -> Rung-2 always
// abstains (§10.2 spec note); stall detection leans on Rung-3 + Rung-4 alone.
func TestRung2_SpecFlowForcesAbstain(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	if _, err := st.SeedSpecJob(ctx, SeedSpecParams{ID: "sp", ChatRef: "c", Now: now}); err != nil {
		t.Fatalf("seed spec: %v", err)
	}
	// drive it to spec_authoring with an active lease.
	ls, err := st.ClaimSpecAuthor(ctx, ClaimSpecAuthorParams{
		JobID: "sp", LeaseID: "l", Identity: "a", ModelFamily: "opus",
		Attested: []string{"role:spec_author", "model_family:opus"}, TTL: time.Hour, Now: now,
	})
	if err != nil {
		t.Fatalf("claim spec author: %v", err)
	}
	_ = ls
	// even if we (wrongly) had Domain-B facts, a spec job forces abstain.
	v, err := st.rung2Evaluate(ctx, DBFactSource{DB: st.DB}, "sp", now, liveCfg)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if v != liveness.Rung2Abstain {
		t.Fatalf("spec-flow must force Rung-2 abstain, got %s", v)
	}
}

// TestEvaluateLiveness_AbstainSurvivesSoftDeadline: the store-level proof that a
// soft deadline + abstain is a no-op (the engine wired through the runtime).
func TestEvaluateLiveness_AbstainSurvivesSoftDeadline(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	epoch := seedLeased(t, st, "j1", now, liveCfg)
	past := now.Add(11 * time.Minute) // past soft (10), before cap (60)
	res, err := st.EvaluateLiveness(ctx, "j1", past, liveCfg, false)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if res.Killed {
		t.Fatalf("soft deadline + Rung-2 abstain must not kill")
	}
	if j, _ := st.GetJob(ctx, "j1"); j.State != job.StateLeased || j.LeaseEpoch != epoch {
		t.Fatalf("the lease must be intact, got %s e%d", j.State, j.LeaseEpoch)
	}
}

// TestFireLeaseDeadline_StaleTimerNoOp: a deadline timer whose epoch is stale (the
// job was re-claimed) is a no-op (the §3.5 epoch-guard).
func TestFireLeaseDeadline_StaleTimerNoOp(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	seedLeased(t, st, "j1", now, liveCfg)
	// a timer armed at a now-stale epoch (the job's live epoch is 1).
	res, err := st.FireLeaseDeadline(ctx, DueTimer{
		ID: "stale", JobID: "j1", Kind: TimerLeaseDeadline, ExpectedEpoch: 99,
	}, now.Add(2*time.Hour), liveCfg, false)
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if res.Killed {
		t.Fatalf("a stale-epoch deadline timer must be a no-op")
	}
}
