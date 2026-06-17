package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// seedWorker registers a live worker row (attestation path inserts one in prod) and
// stamps its last_seen, so the roster counts it as live for the escalation gate.
func seedWorker(t *testing.T, st *store.Store, identity string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO workers (worker_id, identity, host, claimed_capabilities, attested_capabilities, attestation_expires_at, last_seen_at)
		VALUES (?, ?, 'h', '[]', '["role:eng_worker"]', ?, ?)
		ON CONFLICT(worker_id) DO NOTHING`,
		identity, identity, now.Add(time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert worker: %v", err)
	}
	if err := st.RecordWorkerSeen(ctx, identity, now); err != nil {
		t.Fatal(err)
	}
}

// TestWatchdogResyncsWedgedCaps is the #2217 regression at the watchdog layer: a
// `ready` build whose PROJECTION required_capabilities drifted to the reviewer cap
// (no builder can claim it) but whose LEDGER folds to role:eng_worker is self-healed —
// the watchdog re-folds the ledger and rewrites the projection to match it. This makes
// the wedge class unreachable even if some future projection write reintroduces it.
func TestWatchdogResyncsWedgedCaps(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// corrupt ONLY the projection (no ledger event) — exactly the #2217 divergence: a
	// `ready` job the ledger says is a builder job but the table says needs a reviewer.
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET required_capabilities='["role:code_reviewer"]' WHERE id='j'`); err != nil {
		t.Fatal(err)
	}

	rep, err := st.ReconcileStuck(ctx, now, 90*time.Second, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Resynced != 1 {
		t.Fatalf("Resynced=%d, want 1 (the wedge must self-heal)", rep.Resynced)
	}
	j, _ := st.GetJob(ctx, "j")
	if len(j.RequiredCapabilities) != 1 || j.RequiredCapabilities[0] != "role:eng_worker" {
		t.Fatalf("after resync caps=%v, want [role:eng_worker] (claimable)", j.RequiredCapabilities)
	}
	if j.State != job.StateReady {
		t.Fatalf("resync must not change state: got %s", j.State)
	}
	// idempotent: a second pass finds nothing to fix.
	rep2, _ := st.ReconcileStuck(ctx, now, 90*time.Second, 30*time.Minute)
	if rep2.Resynced != 0 {
		t.Fatalf("second pass Resynced=%d, want 0 (already in sync)", rep2.Resynced)
	}
}

// TestWatchdogEscalatesStalledWithLiveFleet: a leasable job that re-folds clean but
// has sat unclaimed past the stall window WHILE live workers exist (a no-eligible
// dead-end) is moved to needs_human so a human always eventually sees it.
func TestWatchdogEscalatesStalledWithLiveFleet(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(100000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "s", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// stall it: updated_at an hour in the past, no lease.
	stale := now.Add(-1 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET updated_at=? WHERE id='s'`, stale); err != nil {
		t.Fatal(err)
	}

	// no live workers -> a down fleet is NOT escalated (the fleet-health watchdog owns that).
	rep, err := st.ReconcileStuck(ctx, now, 90*time.Second, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Escalated != 0 {
		t.Fatalf("down fleet: Escalated=%d, want 0 (do not escalate when nobody could claim)", rep.Escalated)
	}

	// a live worker exists -> the stalled, unclaimable job escalates.
	seedWorker(t, st, "feller-builder-1", now)
	rep, err = st.ReconcileStuck(ctx, now, 90*time.Second, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Escalated != 1 {
		t.Fatalf("live fleet + stalled: Escalated=%d, want 1", rep.Escalated)
	}
	j, _ := st.GetJob(ctx, "s")
	if j.State != job.StateNeedsHuman {
		t.Fatalf("stalled job state=%s, want needs_human", j.State)
	}
}

// TestWatchdogLeavesFreshJobsAlone: a leasable job in sync with its ledger and
// recently touched (an actively-polled build/review) is NOT escalated, even with a
// live fleet — the backstop must never punish legitimately-slow work.
func TestWatchdogLeavesFreshJobsAlone(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(100000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "f", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	seedWorker(t, st, "feller-builder-1", now)
	// updated_at is fresh (within the window).
	fresh := now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET updated_at=? WHERE id='f'`, fresh); err != nil {
		t.Fatal(err)
	}
	rep, err := st.ReconcileStuck(ctx, now, 90*time.Second, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Escalated != 0 || rep.Resynced != 0 {
		t.Fatalf("fresh in-sync job: report=%+v, want all zero", rep)
	}
	j, _ := st.GetJob(ctx, "f")
	if j.State != job.StateReady {
		t.Fatalf("fresh job state=%s, want ready (untouched)", j.State)
	}
}
