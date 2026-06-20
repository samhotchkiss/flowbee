package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestRefreshStaleReadyBuilds reproduces the audit's Finding 1: a build still `blocked`
// when its sibling merges is skipped by the merge-time base refresh (state='ready' only),
// and the dep-clear that later arms it to `ready` does NOT refresh the base — so it would
// dispatch a wasted build cut from stale pre-merge code. The tick-driven
// RefreshStaleReadyBuilds aligns it to the live tip BEFORE dispatch. The fix must also be
// fold-consistent, idempotent, and must never blank a base on a missing tip.
func TestRefreshStaleReadyBuilds(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(4000, 0)

	// A (ready) and B (blocked on A), both adopted at the OLD base S0.
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "A", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, BaseSHA: "S0", Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "B", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, BaseSHA: "S0",
		BlockedBy: []string{"A"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}

	// drive A through review_pending to done -> B's predecessor clears. B goes
	// blocked -> ready, but its base stays S0 (the dep-clear does not refresh it; the
	// merge-time refresh skipped it as `blocked`).
	la, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "A", LeaseID: "LA", Identity: "w", ModelFamily: "claude",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker"}, TTL: time.Minute, Now: now,
	})
	if err != nil {
		t.Fatalf("claim A: %v", err)
	}
	if _, err := st.Result(ctx, store.ResultParams{JobID: "A", Epoch: la.Epoch, Now: now}); err != nil {
		t.Fatalf("result A: %v", err)
	}
	if _, err := st.CompleteJob(ctx, store.CompleteParams{JobID: "A", Now: now}); err != nil {
		t.Fatalf("complete A: %v", err)
	}
	b, _ := st.GetJob(ctx, "B")
	if b.State != job.StateReady {
		t.Fatalf("B state=%s, want ready (deps cleared)", b.State)
	}
	if b.BaseSHA != "S0" {
		t.Fatalf("precondition: B base=%q, want stale S0 (the gap this test closes)", b.BaseSHA)
	}

	// the tick aligns B to the live tip S1 BEFORE a worker ever cuts the worktree.
	n, err := st.RefreshStaleReadyBuilds(ctx, "", "S1", now)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if n != 1 {
		t.Fatalf("refreshed %d, want 1 (only the stale ready build B)", n)
	}
	if b, _ = st.GetJob(ctx, "B"); b.BaseSHA != "S1" {
		t.Fatalf("B base=%q after refresh, want S1 (current tip)", b.BaseSHA)
	}
	// base_sha is a folded field — a rebuild-from-ledger must reproduce the advance.
	assertFoldMatchesProjection(t, st, "B")

	// idempotent: a second pass at the same tip advances nothing (no churn/spurious events).
	if n, err = st.RefreshStaleReadyBuilds(ctx, "", "S1", now); err != nil || n != 0 {
		t.Fatalf("idempotent refresh: n=%d err=%v, want 0/nil", n, err)
	}

	// guard: a missing tip (mirror not yet fetched) must NOT blank the base.
	if n, err = st.RefreshStaleReadyBuilds(ctx, "", "", now); err != nil || n != 0 {
		t.Fatalf("empty-tip refresh: n=%d err=%v, want 0/nil (never blank a base)", n, err)
	}
	if b, _ = st.GetJob(ctx, "B"); b.BaseSHA != "S1" {
		t.Fatalf("empty-tip refresh mutated base to %q, want S1 untouched", b.BaseSHA)
	}
}
