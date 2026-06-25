package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestRouteBuildConflictDivertsToResolver: a BUILD whose worker-side rebase hit a real
// conflict (its branch patch doesn't apply onto the granted base) is diverted to the
// conflict_resolver path — carrying the worker-supplied branch diff as the job's patch so
// the resolver can re-apply + resolve it — instead of the build burning attempts to
// needs_human where the conflict is never resolved.
func TestRouteBuildConflictDivertsToResolver(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "b", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "oldbase", RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// the worker holds a live build lease (state=leased, epoch=3).
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='leased', lease_epoch=3, lease_id='L', bound_identity='w' WHERE id='b'`); err != nil {
		t.Fatal(err)
	}

	const branchDiff = "diff --git a/x b/x\n+conflicting change"
	if err := st.RouteBuildConflict(ctx, store.RouteBuildConflictParams{
		JobID: "b", Epoch: 3, NewBaseSHA: "newmain", BranchDiff: branchDiff, Now: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("route: %v", err)
	}

	j, _ := st.GetJob(ctx, "b")
	if j.State != job.StateResolvingConflict {
		t.Fatalf("state=%s, want resolving_conflict", j.State)
	}
	if j.Role != job.RoleConflictResolver {
		t.Fatalf("role=%s, want conflict_resolver", j.Role)
	}
	if len(j.RequiredCapabilities) != 1 || j.RequiredCapabilities[0] != "role:conflict_resolver" {
		t.Fatalf("caps=%v, want [role:conflict_resolver]", j.RequiredCapabilities)
	}
	if j.BaseSHA != "newmain" {
		t.Fatalf("base_sha=%s, want newmain", j.BaseSHA)
	}
	// the branch diff is stored as the job's patch (the resolver's Context.Diff reads it).
	if d, err := st.JobPatchDiff(ctx, "b"); err != nil || d != branchDiff {
		t.Fatalf("patch_diff=%q err=%v, want the branch diff", d, err)
	}
	// epoch bumped (the reporting worker's lease fenced).
	if j.LeaseEpoch != 4 {
		t.Fatalf("lease_epoch=%d, want 4 (bumped to fence the reporter)", j.LeaseEpoch)
	}
	if k := lastEventKind(ctx, t, st, "b"); k != ledger.KindConflictDetected {
		t.Fatalf("last event=%s, want conflict_detected", k)
	}
}

// A stale epoch (the worker already lost its lease) is rejected with ErrStaleEpoch and the
// job is untouched — the divert must not act on a fenced report.
func TestRouteBuildConflictFencesStaleEpoch(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "b", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='leased', lease_epoch=5 WHERE id='b'`); err != nil {
		t.Fatal(err)
	}
	err := st.RouteBuildConflict(ctx, store.RouteBuildConflictParams{
		JobID: "b", Epoch: 4, NewBaseSHA: "m", BranchDiff: "d", Now: now,
	})
	if err != lease.ErrStaleEpoch {
		t.Fatalf("err=%v, want ErrStaleEpoch", err)
	}
	if j, _ := st.GetJob(ctx, "b"); j.State != job.StateLeased {
		t.Fatalf("stale report mutated the job to %s", j.State)
	}
}

// A job no longer in an active build lease (already re-armed/revoked) is a no-op — the
// divert is idempotent and never clobbers a job that moved on.
func TestRouteBuildConflictNoopWhenNotLeased(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "b", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// ready (not leased), epoch 0 — the worker's report arrives after a revoke.
	if err := st.RouteBuildConflict(ctx, store.RouteBuildConflictParams{
		JobID: "b", Epoch: 0, NewBaseSHA: "m", BranchDiff: "d", Now: now,
	}); err != nil {
		t.Fatalf("noop route: %v", err)
	}
	if j, _ := st.GetJob(ctx, "b"); j.State != job.StateReady {
		t.Fatalf("noop route changed a ready job to %s", j.State)
	}
}

func lastEventKind(ctx context.Context, t *testing.T, st *store.Store, jobID string) ledger.EventKind {
	t.Helper()
	evs, err := st.LoadEvents(ctx, jobID)
	if err != nil || len(evs) == 0 {
		t.Fatalf("load events: %v", err)
	}
	return evs[len(evs)-1].Kind
}
