package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestRouteMergeConflictDivertsToResolver: a job whose MERGE conflicted (a sibling
// merged into the same area after the verdict minted) is diverted to the
// conflict_resolver path at the current main tip — its stale verdict invalidated —
// instead of the project-out sender retrying the doomed merge forever.
func TestRouteMergeConflictDivertsToResolver(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "m", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "oldbase", RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// in `merging` with a minted (now stale) verdict, like a job whose merge failed.
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='merging', verdict='{"decision":"approved"}' WHERE id='m'`); err != nil {
		t.Fatal(err)
	}

	if err := st.RouteMergeConflict(ctx, "m", "newmain", now.Add(time.Second)); err != nil {
		t.Fatalf("route: %v", err)
	}
	j, _ := st.GetJob(ctx, "m")
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
		t.Fatalf("base_sha=%s, want newmain (resolve against current main)", j.BaseSHA)
	}
	if j.Verdict != nil {
		t.Fatalf("stale verdict not cleared: %+v", j.Verdict)
	}
	// a KindConflictDetected event was recorded (so the fold reproduces the divert).
	events, _ := st.LoadEvents(ctx, "m")
	if k := events[len(events)-1].Kind; k != ledger.KindConflictDetected {
		t.Fatalf("last event=%s, want conflict_detected", k)
	}

	// idempotent: a job no longer in merging/mergeable (already merged) is left alone.
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='done' WHERE id='m'`); err != nil {
		t.Fatal(err)
	}
	if err := st.RouteMergeConflict(ctx, "m", "x", now.Add(2*time.Second)); err != nil {
		t.Fatalf("idempotent route: %v", err)
	}
	if j2, _ := st.GetJob(ctx, "m"); j2.State != job.StateDone {
		t.Fatalf("idempotent route changed a done job to %s", j2.State)
	}
}
