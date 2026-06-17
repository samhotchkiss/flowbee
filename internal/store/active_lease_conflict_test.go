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
