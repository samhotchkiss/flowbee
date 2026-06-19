package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestActiveLeaseJobsIncludesResolvingConflict: a resolving_conflict job holding a lease
// must appear in ActiveLeaseJobs, so the heartbeat-miss liveness sweep reaps its lease
// when the resolver dies (a crash, or the CP restarting mid-resolve). Without it the
// orphaned lease stays held until the 20-min TTL — a job stranded far too long.
func TestActiveLeaseJobsIncludesResolvingConflict(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "c", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='resolving_conflict', role='conflict_resolver',
		    lease_id='L', lease_epoch=1, bound_identity='r' WHERE id='c'`); err != nil {
		t.Fatal(err)
	}

	ids, err := st.ActiveLeaseJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		if id == "c" {
			found = true
		}
	}
	if !found {
		t.Fatal("a resolving_conflict job holding a lease must be in ActiveLeaseJobs (else its orphaned lease is never reaped on heartbeat-miss)")
	}
}

// TestActiveLeaseJobsExcludesMergeHandoff: a merge_handoff job is parked awaiting a
// HUMAN merge — it holds the one-active-lease uniqueness slot but has NO bound worker,
// so the liveness/two-rung-stall sweep must NOT evaluate it. Including it made the stall
// ladder read its stale build-phase heartbeat as a stall and escalate a healthy handoff
// to needs_human minutes after the PR opened (the live #175/#177 regression). A human
// merge gate can sit open for hours; the board + reconcile surface it, not the watchdog.
func TestActiveLeaseJobsExcludesMergeHandoff(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "h", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// merge_handoff: approved + PR open, NO bound worker (the human gate). A stale
	// last_heartbeat_at lingers from the build phase — exactly the input that tripped
	// the false stall.
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='merge_handoff', role='code_reviewer',
		    lease_id=NULL, bound_identity=NULL, last_heartbeat_at='2000-01-01T00:00:00Z'
		 WHERE id='h'`); err != nil {
		t.Fatal(err)
	}

	ids, err := st.ActiveLeaseJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == "h" {
			t.Fatal("a merge_handoff job (awaiting human merge, no worker) must NOT be in ActiveLeaseJobs — else the stall ladder escalates a healthy handoff to needs_human")
		}
	}
}

// TestFireLeaseDeadlineSkipsMergeHandoff covers the TIMER path of the handoff loop
// (the one the first fix missed): a lease/phase-deadline timer left over from the build
// phase fires AFTER the job reached merge_handoff. The timer guard must skip it — a
// merge_handoff job is parked for a human, not a dead worker — or the timer revokes the
// handoff back to ready and the build loops forever (live #175/#177). Uses an aggressive
// reap config to prove the guard short-circuits BEFORE any staleness evaluation.
func TestFireLeaseDeadlineSkipsMergeHandoff(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(100000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "h", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	// merge_handoff at epoch 3, with a stale build-phase heartbeat — exactly the input
	// that looped the live handoffs.
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='merge_handoff', lease_epoch=3,
		    last_heartbeat_at='2000-01-01T00:00:00Z', lease_id=NULL, bound_identity=NULL WHERE id='h'`); err != nil {
		t.Fatal(err)
	}

	// a leftover deadline timer for the SAME epoch (so the epoch guard passes and we
	// exercise the state guard). Aggressive reap/cap that WOULD kill a worker-leased job.
	cfg := store.LivenessConfig{HeartbeatReapAfter: time.Second, AbsoluteCap: time.Second}
	res, err := st.FireLeaseDeadline(ctx, store.DueTimer{ID: "t1", JobID: "h", ExpectedEpoch: 3, Kind: store.TimerPhaseDeadline}, now, cfg, false)
	if err != nil {
		t.Fatalf("FireLeaseDeadline: %v", err)
	}
	if res.Killed {
		t.Fatal("a merge_handoff job must NOT be killed by a leftover deadline timer (the handoff loop)")
	}
	j, _ := st.GetJob(ctx, "h")
	if j.State != job.StateMergeHandoff {
		t.Fatalf("merge_handoff job was disturbed by the timer: state=%s (want merge_handoff)", j.State)
	}
}

// TestLivenessExcludesMerging: a `merging` job (a merge dispatched to the project-OUT
// outbox — no bound worker) must be excluded from BOTH liveness entry points, exactly
// like merge_handoff and exactly as the reconcile-side supersedable() already excludes
// it. Reaping a merge in flight would yank the job back to build mid-dispatch while the
// merge outbox row is still pending — a double-action. Recovery is the outbox +
// reconcile, never the liveness ladder.
func TestLivenessExcludesMerging(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "m", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='merging', lease_epoch=2, last_heartbeat_at='2000-01-01T00:00:00Z',
		    lease_id=NULL, bound_identity=NULL WHERE id='m'`); err != nil {
		t.Fatal(err)
	}

	// (1) poller scope
	ids, err := st.ActiveLeaseJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == "m" {
			t.Fatal("a merging job must NOT be in ActiveLeaseJobs (the mid-dispatch yank)")
		}
	}
	// (2) timer scope — a leftover deadline timer must not reap it
	cfg := store.LivenessConfig{HeartbeatReapAfter: time.Second, AbsoluteCap: time.Second}
	res, err := st.FireLeaseDeadline(ctx, store.DueTimer{ID: "tm", JobID: "m", ExpectedEpoch: 2, Kind: store.TimerPhaseDeadline}, time.Unix(100000, 0), cfg, false)
	if err != nil {
		t.Fatalf("FireLeaseDeadline: %v", err)
	}
	if res.Killed {
		t.Fatal("a merging job must NOT be killed by a leftover deadline timer")
	}
	if j, _ := st.GetJob(ctx, "m"); j.State != job.StateMerging {
		t.Fatalf("merging job disturbed: state=%s want merging", j.State)
	}
}
