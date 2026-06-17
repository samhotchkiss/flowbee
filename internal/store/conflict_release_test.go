package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// putResolvingConflict drives a job into resolving_conflict with a bound conflict_resolver
// lease at the given attempts, mirroring a claimed F8 resolution job.
func putResolvingConflict(t *testing.T, st *store.Store, id string, attempts int, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs SET state='resolving_conflict', role='conflict_resolver',
		    required_capabilities='["role:conflict_resolver"]', stage='resolve_conflict',
		    lease_epoch=1, lease_id='L-`+id+`', bound_identity='resolver-x', attempts=?
		 WHERE id=?`, attempts, id); err != nil {
		t.Fatal(err)
	}
}

// TestConflictReleaseReArmsToGateNotReady is the regression for the live wedge: a
// conflict_resolver that releases a resolving_conflict lease (gave up / timed out / no
// resolution) must re-arm BACK to resolving_conflict — re-claimable by another
// conflict_resolver — NOT to `ready`, which (carrying role:conflict_resolver caps) no
// eng_worker can claim and no conflict_resolver looks at: a hard wedge.
func TestConflictReleaseReArmsToGateNotReady(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	putResolvingConflict(t, st, "cr", 0, now)
	if err := st.Release(ctx, store.ReleaseParams{JobID: "cr", Epoch: 1, Now: now.Add(time.Second)}); err != nil {
		t.Fatalf("release: %v", err)
	}
	j, _ := st.GetJob(ctx, "cr")
	if j.State != job.StateResolvingConflict {
		t.Fatalf("released conflict job state=%s, want resolving_conflict (NOT ready — that wedges)", j.State)
	}
	if len(j.RequiredCapabilities) != 1 || j.RequiredCapabilities[0] != "role:conflict_resolver" {
		t.Fatalf("caps=%v, want [role:conflict_resolver] so a resolver can re-claim", j.RequiredCapabilities)
	}
	if j.LeaseID != "" {
		t.Fatalf("lease not released: %q", j.LeaseID)
	}
	if j.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1 (a failed resolution burns an attempt)", j.Attempts)
	}
}

// TestConflictReleaseEscalatesOnExhaustion: a resolver that keeps failing must not
// churn forever — the release that exhausts max_attempts escalates to needs_human.
func TestConflictReleaseEscalatesOnExhaustion(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	// attempts=4, max=5 (default): this release burns the final attempt.
	putResolvingConflict(t, st, "cx", 4, now)
	if err := st.Release(ctx, store.ReleaseParams{JobID: "cx", Epoch: 1, Now: now.Add(time.Second)}); err != nil {
		t.Fatalf("release: %v", err)
	}
	j, _ := st.GetJob(ctx, "cx")
	if j.State != job.StateNeedsHuman {
		t.Fatalf("exhausted conflict resolution state=%s, want needs_human", j.State)
	}
	if j.EscalationReason != string(job.EscalationAttempts) {
		t.Fatalf("escalation_reason=%q, want attempts", j.EscalationReason)
	}
}
