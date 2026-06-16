package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestReleaseNoPenalty: a build abandon normally burns an attempt, but a NoPenalty
// release (the worker built fine but lost a fast-forward race) re-arms to ready
// WITHOUT burning — so re-validation churn can't exhaust max_attempts and escalate a
// good change to needs_human.
func TestReleaseNoPenalty(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)

	leaseBuild := func(t *testing.T, st *store.Store) (string, int) {
		t.Helper()
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: "b", Kind: job.KindBuild, Flow: "build", Stage: "build",
			Role: job.RoleEngWorker, BaseSHA: "base", Now: now,
		}); err != nil {
			t.Fatal(err)
		}
		ls, err := st.ClaimReadyJob(ctx, store.ClaimParams{
			JobID: "b", LeaseID: "L1", Identity: "w", ModelFamily: "claude",
			Role: job.RoleEngWorker, TTL: time.Minute, Now: now,
		})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return "b", ls.Epoch
	}

	// penalty release (default): attempts increments.
	st := testutil.NewStore(t)
	_, epoch := leaseBuild(t, st)
	if err := st.Release(ctx, store.ReleaseParams{JobID: "b", Epoch: epoch, Now: now}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if j, _ := st.GetJob(ctx, "b"); j.Attempts != 1 {
		t.Fatalf("penalty release: attempts=%d, want 1", j.Attempts)
	}

	// no-penalty release: attempts stays 0, still re-arms to ready.
	st2 := testutil.NewStore(t)
	_, epoch2 := leaseBuild(t, st2)
	if err := st2.Release(ctx, store.ReleaseParams{JobID: "b", Epoch: epoch2, Now: now, NoPenalty: true}); err != nil {
		t.Fatalf("no-penalty release: %v", err)
	}
	j, _ := st2.GetJob(ctx, "b")
	if j.Attempts != 0 {
		t.Fatalf("no-penalty release: attempts=%d, want 0 (no burn)", j.Attempts)
	}
	if j.State != job.StateReady {
		t.Fatalf("no-penalty release: state=%s, want ready (still re-armed)", j.State)
	}
}
