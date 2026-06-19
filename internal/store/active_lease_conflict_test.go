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
