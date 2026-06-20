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

// TestOnlyActiveBuildsHoldReservation pins the russ #213 fix: a reservation bites ONLY
// while a build is actively producing a diff (leased/building/resolving_conflict). Every
// POST-build state (review_pending/code_review/mergeable/merging/merge_handoff) has an open
// PR and a done build, so it must NOT reserve — else a few reviewed PRs on hot shared files
// withhold every overlapping ready build and the fleet starves (8 ready / 14 idle / 0
// building). End-to-end: a ready build overlapping a review_pending job is STILL leasable.
func TestOnlyActiveBuildsHoldReservation(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Unix(1000, 0)

	const hot = `{"paths":["backend/internal/tasks/flow_templates.go"],"scope":"worktree"}`
	const cold = `{"paths":["backend/internal/unrelated/other.go"],"scope":"worktree"}`
	seed := func(id, state, blast string) {
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
			RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx,
			`UPDATE jobs SET state=?, declared_blast_radius=? WHERE id=?`, state, blast, id); err != nil {
			t.Fatal(err)
		}
	}

	reserves := map[string]bool{"leased": true, "building": true, "resolving_conflict": true}
	// actively-building states declare `cold` (so they don't overlap the `hot` ready build
	// below); post-build states declare `hot` — they must NOT reserve regardless.
	for _, st8 := range []string{
		"leased", "building", "resolving_conflict",
		"review_pending", "code_review", "mergeable", "merging", "merge_handoff",
	} {
		blast := hot
		if reserves[st8] {
			blast = cold
		}
		seed("j_"+st8, st8, blast)
	}
	resv, err := st.ActiveReservations(ctx)
	if err != nil {
		t.Fatalf("ActiveReservations: %v", err)
	}
	held := map[string]bool{}
	for _, r := range resv {
		held[r.JobID] = true
	}
	for _, st8 := range []string{"leased", "building", "resolving_conflict", "review_pending", "code_review", "mergeable", "merging", "merge_handoff"} {
		want := reserves[st8]
		if got := held["j_"+st8]; got != want {
			t.Errorf("state %q holds reservation=%v, want %v (only actively-building states reserve)", st8, got, want)
		}
	}

	// end-to-end: a ready build that overlaps a review_pending job on the hot file is NOT
	// withheld — the starvation the fix cures (before: the review_pending reservation hid it).
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "ready1", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET declared_blast_radius=? WHERE id='ready1'`, hot); err != nil {
		t.Fatal(err)
	}
	cands, err := st.ReadyCandidatesReserved(ctx)
	if err != nil {
		t.Fatalf("ReadyCandidatesReserved: %v", err)
	}
	leasable := false
	for _, c := range cands {
		if c.JobID == "ready1" {
			leasable = true
		}
	}
	if !leasable {
		t.Fatal("a ready build overlapping only a review_pending job must be leasable — review_pending must not reserve (the #213 starvation)")
	}
}
