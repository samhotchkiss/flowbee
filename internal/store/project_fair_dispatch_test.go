package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/scheduler"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestProjectFairLeaseAndCreditSurviveRestart(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "fair.db")
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(90_000, 0)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail", State: "active", SchedulerWeight: 3}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "mail-job", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET project_id='mail' WHERE id='mail-job'`); err != nil {
		t.Fatal(err)
	}

	snap, err := st.LoadProjectFairSnapshot(ctx, scheduler.PoolBuild)
	if err != nil {
		t.Fatal(err)
	}
	cands, err := st.ReadyCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	turn := scheduler.PickProjectFair(cands, snap.Policies, snap.Active, snap.FairState, scheduler.FairConfig{Pool: scheduler.PoolBuild, Now: now})
	if !turn.OK {
		t.Fatal("no fair turn")
	}
	claim := &store.ProjectFairClaim{Pool: scheduler.PoolBuild, ProjectID: turn.WinningProject, JobID: turn.Selected.JobID, ForcedByAge: turn.ForcedByAge, NextState: turn.NextState, Decisions: turn.Decisions, Now: now}
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{JobID: "mail-job", LeaseID: "lease-mail", Identity: "builder", ModelFamily: "codex", Role: job.RoleEngWorker, TTL: time.Minute, Now: now, Fair: claim}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	restarted, err := st.LoadProjectFairSnapshot(ctx, scheduler.PoolBuild)
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.FairState.LastServedByPool[scheduler.PoolBuild]["mail"]; !got.Equal(now) {
		t.Fatalf("last service lost across restart: %v", got)
	}
	if restarted.Active["mail"] != 1 {
		t.Fatalf("active occupancy after restart=%d want 1", restarted.Active["mail"])
	}
	last, err := st.LastProjectSchedulerTurn(ctx, scheduler.PoolBuild)
	if err != nil || last.JobID != "mail-job" {
		t.Fatalf("durable turn=%+v err=%v", last, err)
	}
}

func TestProjectFairWhyNotAndConcurrencyFenceAreDurable(t *testing.T) {
	ctx := context.Background()
	// This test uses the package's normal migrated store helper indirectly via a
	// temporary file so the claim-time cap and persisted explanation share the
	// same production transaction path.
	dsn := filepath.Join(t.TempDir(), "cap.db")
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Unix(91_000, 0)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "cap", Name: "Cap", State: "active", SchedulerWeight: 1, ConcurrencyCap: 1}, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err := st.SeedJob(ctx, store.SeedParams{ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET project_id='cap' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ := st.LoadProjectFairSnapshot(ctx, scheduler.PoolBuild)
	cands, _ := st.ReadyCandidates(ctx)
	turn := scheduler.PickProjectFair(cands, snap.Policies, snap.Active, snap.FairState, scheduler.FairConfig{Pool: scheduler.PoolBuild, Now: now})
	fair := &store.ProjectFairClaim{Pool: scheduler.PoolBuild, ProjectID: turn.WinningProject, JobID: turn.Selected.JobID, NextState: turn.NextState, Decisions: turn.Decisions, Now: now}
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{JobID: turn.Selected.JobID, LeaseID: "cap-first", Identity: "w1", ModelFamily: "codex", Role: job.RoleEngWorker, TTL: time.Minute, Now: now, Fair: fair}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{JobID: "two", LeaseID: "cap-second", Identity: "w2", ModelFamily: "codex", Role: job.RoleEngWorker, TTL: time.Minute, Now: now}); err == nil {
		t.Fatal("claim-time concurrency fence allowed a second active lease")
	}
	last, err := st.LastProjectSchedulerTurn(ctx, scheduler.PoolBuild)
	if err != nil || len(last.Decisions) != 2 {
		t.Fatalf("why-not shadow missing: %+v err=%v", last, err)
	}
}
