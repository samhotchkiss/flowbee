package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// seedReadyBuildEpoch seeds a minimal ready build job for the epoch unit tests.
func seedReadyBuildEpoch(t *testing.T, ctx context.Context, st *store.Store, id string) {
	t.Helper()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base-0",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// TestPromoteResultValidatesLiveEpoch proves §6.5.1: only the LIVE lease epoch is
// promoted; a stale epoch is orphaned (never promoted), and build_epoch tracks the
// live one. The mirror is nil here (no git fixture) — the epoch validation + the
// (job, epoch) bookkeeping still run.
func TestPromoteResultValidatesLiveEpoch(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedReadyBuildEpoch(t, ctx, st, "j1")

	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "j1", LeaseID: "L1", Identity: "w", ModelFamily: "codex",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker"},
		TTL: time.Minute, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	j, _ := st.GetJob(ctx, "j1")
	live := j.LeaseEpoch

	// a STALE epoch (live-1) is never promoted; build_epoch stays 0.
	promoted, err := st.PromoteResult(ctx, nil, "j1", live-1, "", time.Unix(1001, 0))
	if err != nil {
		t.Fatalf("promote stale: %v", err)
	}
	if promoted {
		t.Fatalf("a stale epoch must not be promoted")
	}
	if jj, _ := st.GetJob(ctx, "j1"); jj.BuildEpoch != 0 {
		t.Fatalf("build_epoch must stay 0 after a stale-epoch promote, got %d", jj.BuildEpoch)
	}

	// the LIVE epoch promotes; build_epoch becomes the live epoch.
	promoted, err = st.PromoteResult(ctx, nil, "j1", live, "", time.Unix(1002, 0))
	if err != nil || !promoted {
		t.Fatalf("live promote: promoted=%v err=%v", promoted, err)
	}
	if jj, _ := st.GetJob(ctx, "j1"); jj.BuildEpoch != live {
		t.Fatalf("build_epoch must be the live epoch %d, got %d", live, jj.BuildEpoch)
	}
}

// TestLiveEpochCIGatesStaleZombie proves §6.5.2: the live gate reads ONLY the live
// build epoch's CI. A zombie that turned its STALE epoch's CI green can never satisfy
// the live gate.
func TestLiveEpochCIGatesStaleZombie(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedReadyBuildEpoch(t, ctx, st, "j2")
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "j2", LeaseID: "L1", Identity: "w", ModelFamily: "codex",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker"},
		TTL: time.Minute, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	j, _ := st.GetJob(ctx, "j2")
	deadEpoch := j.LeaseEpoch

	// the zombie's STALE epoch CI goes green.
	if err := st.RecordEpochCI(ctx, "j2", deadEpoch, "zombie-sha", store.EpochCISuccess, time.Unix(1001, 0)); err != nil {
		t.Fatalf("record dead CI: %v", err)
	}
	// the live build epoch (after a live re-dispatch + promotion) is different.
	liveEpoch := deadEpoch + 1
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET lease_epoch = ? WHERE id = 'j2'`, liveEpoch); err != nil {
		t.Fatalf("bump epoch: %v", err)
	}
	if _, err := st.PromoteResult(ctx, nil, "j2", liveEpoch, "", time.Unix(1002, 0)); err != nil {
		t.Fatalf("promote live: %v", err)
	}

	// the live gate is RED: the live epoch has no green CI, only the dead one does.
	green, err := st.LiveEpochCIGreen(ctx, "j2")
	if err != nil {
		t.Fatalf("live CI: %v", err)
	}
	if green {
		t.Fatalf("a zombie's stale-epoch green CI must NOT satisfy the live gate")
	}
	// once the LIVE epoch's CI goes green, the gate is satisfiable.
	if err := st.RecordEpochCI(ctx, "j2", liveEpoch, "live-sha", store.EpochCISuccess, time.Unix(1003, 0)); err != nil {
		t.Fatalf("record live CI: %v", err)
	}
	if green, _ := st.LiveEpochCIGreen(ctx, "j2"); !green {
		t.Fatalf("the live epoch's green CI must satisfy the gate")
	}
}

// TestCompensateIsIdempotent proves §6.5.4: compensate(job, dead_epoch) records the
// ref-drop / CI-cancel / draft-back once; a re-run is a no-op (a crash mid-compensate
// replays cleanly). The dead epoch's CI is marked cancelled so the live gate ignores it.
func TestCompensateIsIdempotent(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedReadyBuildEpoch(t, ctx, st, "j3")
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET pr_number = 42 WHERE id = 'j3'`); err != nil {
		t.Fatalf("set pr: %v", err)
	}

	res, err := st.Compensate(ctx, store.CompensateParams{
		JobID: "j3", DeadEpoch: 1, Reason: "absolute_cap",
		Mirror: nil, EnqueueDraftBack: true, Now: time.Unix(2000, 0),
	})
	if err != nil {
		t.Fatalf("compensate: %v", err)
	}
	if !res.CICancelled || !res.PRDrafted {
		t.Fatalf("compensation must cancel CI + draft-back the PR: %+v", res)
	}
	if s, _ := st.EpochCIStateFor(ctx, "j3", 1); s != store.EpochCICancelled {
		t.Fatalf("the dead epoch CI must be cancelled, got %s", s)
	}
	// a draft-back outbox row is enqueued (never leave a ready zombie PR).
	row, ok, err := st.NextPendingOutbox(ctx)
	if err != nil || !ok || row.Action != store.ActionDraftPR {
		t.Fatalf("a draft-back action must be enqueued, ok=%v action=%q err=%v", ok, row.Action, err)
	}

	// re-running compensation for the same (job, dead_epoch) is a no-op.
	res2, err := st.Compensate(ctx, store.CompensateParams{
		JobID: "j3", DeadEpoch: 1, Reason: "absolute_cap", EnqueueDraftBack: true, Now: time.Unix(2001, 0),
	})
	if err != nil {
		t.Fatalf("re-compensate: %v", err)
	}
	if !res2.AlreadyDone {
		t.Fatalf("re-compensation must be a no-op (idempotent), got %+v", res2)
	}
	comp, found, _ := st.CompensationFor(ctx, "j3", 1)
	if !found || !comp.CICancelled {
		t.Fatalf("the compensation record must persist: %+v found=%v", comp, found)
	}
}
