package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// leaseAt drives a seeded build job into 'leased' with the given last-heartbeat age,
// the absolute cap NOT yet hit, and the substate a live worker would have left.
func leaseAt(t *testing.T, st *store.Store, id string, hbAgo time.Duration, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// leased, cap 15 min out (not hit), last heartbeat hbAgo in the past.
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs SET state='leased', lease_epoch=1, lease_id='L-`+id+`', bound_identity='w-`+id+`',
		    lease_deadline=?, agent_health='ok', rung1_class='working', rung2_last_verdict='abstain',
		    last_heartbeat_at=? WHERE id=?`,
		now.Add(15*time.Minute).Format(time.RFC3339Nano),
		now.Add(-hbAgo).Format(time.RFC3339Nano), id); err != nil {
		t.Fatal(err)
	}
}

// TestEvaluateLivenessReapsStaleHeartbeat: a leased job whose worker stopped checking
// in past the reap window is reaped (revoked + re-armed) far before the 20-min absolute
// cap — the crash-recovery fast path. A recently-heartbeating worker is left alone.
func TestEvaluateLivenessReapsStaleHeartbeat(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(2_000_000, 0)
	cfg := store.LivenessConfig{
		AbsoluteCap:        20 * time.Minute,
		PhaseBudget:        10 * time.Minute,
		HeartbeatReapAfter: 4 * time.Minute,
	}

	// (A) stale: last heartbeat 6 min ago (> 4 min reap window), cap NOT hit -> reaped.
	st := testutil.NewStore(t)
	leaseAt(t, st, "dead", 6*time.Minute, now)
	if _, err := st.EvaluateLiveness(ctx, "dead", now, cfg, false); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if j, _ := st.GetJob(ctx, "dead"); j.State == job.StateLeased {
		t.Fatalf("a stale-heartbeat lease must be reaped (not still leased) before the absolute cap")
	}

	// (B) live: last heartbeat 30s ago -> NOT reaped (worker is fine).
	st2 := testutil.NewStore(t)
	leaseAt(t, st2, "live", 30*time.Second, now)
	if _, err := st2.EvaluateLiveness(ctx, "live", now, cfg, false); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if j, _ := st2.GetJob(ctx, "live"); j.State != job.StateLeased {
		t.Fatalf("a recently-heartbeating worker must NOT be reaped, state=%s", j.State)
	}

	// (C) FRESH re-claim: just granted (grant time ~now), but last_heartbeat_at is stale
	// from the PRIOR worker. The grant-time floor must protect it — a new worker gets the
	// full reap window to start beating, NOT reaped on the old worker's silence.
	st3 := testutil.NewStore(t)
	if _, err := st3.SeedJob(ctx, store.SeedParams{
		ID: "fresh", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// lease_deadline = now + AbsoluteCap => grant time == now; last heartbeat 10 min stale.
	if _, err := st3.DB.ExecContext(ctx, `
		UPDATE jobs SET state='leased', lease_epoch=1, lease_id='L-fresh', bound_identity='w-fresh',
		    lease_deadline=?, agent_health='ok', rung1_class='working', rung2_last_verdict='abstain',
		    last_heartbeat_at=? WHERE id='fresh'`,
		now.Add(cfg.AbsoluteCap).Format(time.RFC3339Nano),
		now.Add(-10*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := st3.EvaluateLiveness(ctx, "fresh", now, cfg, false); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if j, _ := st3.GetJob(ctx, "fresh"); j.State != job.StateLeased {
		t.Fatalf("a freshly re-claimed lease must NOT be reaped on the prior worker's stale heartbeat, state=%s", j.State)
	}
}
