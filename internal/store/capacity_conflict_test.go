package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// TestResolvingConflictCountsAgainstSlots: a conflict_resolver is a real running agent,
// so its resolving_conflict lease must count against the box's per-model slot budget.
// The slot-count clause previously omitted resolving_conflict (a hand-copied literal
// that drifted from job.ActiveLeaseStates), so a box busy resolving could still win a
// build claim and overcommit. Now derived from the canonical set, it counts.
func TestResolvingConflictCountsAgainstSlots(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	enrollBox(t, st, "box-1", "ident-1", map[string]int{"claude": 1}, nil)
	attested := []string{"role:eng_worker", "model_family:claude"}

	// the box's one claude slot is held by an active resolving_conflict lease.
	rc := ulid.New()
	seedReadyClaude(t, st, rc)
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='resolving_conflict', bound_identity='ident-1', bound_model_family='claude', lease_id='L-rc' WHERE id=?`, rc); err != nil {
		t.Fatal(err)
	}

	// a 2nd claude build must NOT claim — the box is at claude:1, busy resolving.
	b := ulid.New()
	seedReadyClaude(t, st, b)
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: b, LeaseID: ulid.New(), Identity: "ident-1", ModelFamily: "claude",
		Role: job.RoleEngWorker, Attested: attested, TTL: time.Minute, Now: time.Unix(21, 0),
	}); err == nil {
		t.Fatal("OVERCOMMIT: box won a 2nd claude lease while a resolver held its only slot")
	}
	if j, _ := st.GetJob(ctx, b); j.State != job.StateReady {
		t.Fatalf("blocked build state=%s want ready", j.State)
	}
}

// TestConflictClaimGatedBySlots: the resolver claim itself respects the slot budget —
// a box at its limit cannot be dispatched a conflict resolution (the claim was
// previously ungated entirely, so a resolver landed on a full box).
func TestConflictClaimGatedBySlots(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	enrollBox(t, st, "box-1", "ident-1", map[string]int{"claude": 1}, nil)
	attested := []string{"role:eng_worker", "model_family:claude"}

	// fill the box's one claude slot with a live build lease.
	busy := ulid.New()
	seedReadyClaude(t, st, busy)
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: busy, LeaseID: ulid.New(), Identity: "ident-1", ModelFamily: "claude",
		Role: job.RoleEngWorker, Attested: attested, TTL: time.Minute, Now: time.Unix(20, 0),
	}); err != nil {
		t.Fatalf("seed busy lease: %v", err)
	}

	// an unclaimed conflict awaiting a resolver.
	cf := ulid.New()
	seedReadyClaude(t, st, cf)
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='resolving_conflict', role='conflict_resolver', required_capabilities='["model_family:claude"]', bound_identity=NULL, lease_id=NULL WHERE id=?`, cf); err != nil {
		t.Fatal(err)
	}

	_, err := st.ClaimConflictJob(ctx, store.ClaimConflictParams{
		JobID: cf, LeaseID: ulid.New(), Identity: "ident-1", ModelFamily: "claude",
		Attested: []string{"model_family:claude"}, TTL: time.Minute, Now: time.Unix(21, 0),
	})
	if err == nil {
		t.Fatal("OVERCOMMIT: resolver dispatched onto a box with no free claude slot")
	}
	if err != lease.ErrLostRace && err != store.ErrNoCapacity {
		t.Fatalf("want a capacity/lost-race rejection, got %v", err)
	}
	if j, _ := st.GetJob(ctx, cf); j.State != job.StateResolvingConflict {
		t.Fatalf("ungated conflict claim mutated state to %s", j.State)
	}
}
