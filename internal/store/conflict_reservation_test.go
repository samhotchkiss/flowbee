package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestMergeHandoffDoesNotHoldReservation: a job parked at the human merge gate
// (merge_handoff) must NOT appear in ActiveReservations. With allow_self_merge off it sits
// there indefinitely, and holding its blast-radius reservation that long permanently withholds
// every overlapping ready build — the fleet wedges (the live "everything stopped" incident).
// An actively IN-FLIGHT build (building) with the same declaration MUST still reserve, since
// that is the conflict-avoidance the reservation exists for.
func TestMergeHandoffDoesNotHoldReservation(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Unix(1000, 0)

	seed := func(id, state string) {
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx,
			`UPDATE jobs SET state=?, declared_blast_radius=? WHERE id=?`,
			state, `{"paths":["backend/internal/tasks/tasks.go"],"scope":"worktree"}`, id); err != nil {
			t.Fatal(err)
		}
	}
	seed("parked", "merge_handoff")
	seed("inflight", "building")

	resv, err := st.ActiveReservations(ctx)
	if err != nil {
		t.Fatalf("ActiveReservations: %v", err)
	}
	held := map[string]bool{}
	for _, r := range resv {
		held[r.JobID] = true
	}
	if held["parked"] {
		t.Error("a merge_handoff job must NOT hold a reservation (it parks indefinitely at the human gate and starves overlapping ready builds)")
	}
	if !held["inflight"] {
		t.Error("an in-flight building job MUST still hold its reservation (conflict-avoidance for active builds)")
	}
}
