package ledger

import (
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
)

// TestFoldResetsBuildCapsOnReArmToReady locks the fix for the projection!=Fold churn:
// a build that reached review_pending (caps become role:code_reviewer) and is then
// re-armed to `ready` via a path that ISN'T bounce/supersede — an operator requeue out
// of needs_human (KindStateChanged) — must fold back to the eng_worker build caps. If the
// fold keeps the stale review caps, it diverges from the projection (which
// NormalizeStrandedReadyBuilds pins to eng_worker), and the resync + normalize watchdogs
// repair it in opposite directions forever. A `ready` build is ALWAYS an eng_worker surface.
func TestFoldResetsBuildCapsOnReArmToReady(t *testing.T) {
	now := time.Unix(100, 0)
	events := []Event{
		{JobID: "B1", JobSeq: 1, Kind: KindJobCreated, ToState: job.StateReady, CreatedAt: now,
			Payload: Payload{Kind: job.KindBuild, Role: job.RoleEngWorker, RequiredCapabilities: []string{"role:eng_worker"}}},
		{JobID: "B1", JobSeq: 2, Kind: KindLeaseClaimed, ToState: job.StateLeased, LeaseEpoch: 1, CreatedAt: now},
		{JobID: "B1", JobSeq: 3, Kind: KindResultAccepted, ToState: job.StateReviewPending, CreatedAt: now}, // caps -> code_reviewer
		{JobID: "B1", JobSeq: 4, Kind: KindStateChanged, ToState: job.StateNeedsHuman, CreatedAt: now,
			Payload: Payload{EscalationReason: "attempts"}},
		{JobID: "B1", JobSeq: 5, Kind: KindStateChanged, ToState: job.StateReady, CreatedAt: now,
			Payload: Payload{ResetCounters: true}}, // operator requeue back to ready
	}
	j, err := Fold(events)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != job.StateReady {
		t.Fatalf("state=%v want ready", j.State)
	}
	if j.Role != job.RoleEngWorker {
		t.Errorf("role=%q want eng_worker (a ready build is an eng_worker surface)", j.Role)
	}
	if len(j.RequiredCapabilities) != 1 || j.RequiredCapabilities[0] != "role:eng_worker" {
		t.Errorf("caps=%v want [role:eng_worker] — stale review caps strand the build + churn the watchdogs", j.RequiredCapabilities)
	}
}

func TestFoldHappyPath(t *testing.T) {
	now := time.Unix(100, 0)
	events := []Event{
		{JobID: "J1", JobSeq: 1, Kind: KindJobCreated, ToState: job.StateReady, Actor: "system", CreatedAt: now,
			Payload: Payload{Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, BaseSHA: "base1", Priority: 7}},
		{JobID: "J1", JobSeq: 2, Kind: KindLeaseClaimed, FromState: job.StateReady, ToState: job.StateLeased, LeaseEpoch: 1, Actor: "w1", CreatedAt: now,
			Payload: Payload{LeaseID: "L1", BoundIdentity: "w1", BoundModelFamily: "codex"}},
		{JobID: "J1", JobSeq: 3, Kind: KindWorkerStarted, FromState: job.StateLeased, ToState: job.StateBuilding, LeaseEpoch: 1, Actor: "w1", CreatedAt: now},
		{JobID: "J1", JobSeq: 4, Kind: KindHeartbeat, FromState: job.StateBuilding, ToState: job.StateBuilding, LeaseEpoch: 1, Actor: "w1", CreatedAt: now},
		{JobID: "J1", JobSeq: 5, Kind: KindResultAccepted, FromState: job.StateBuilding, ToState: job.StateReviewPending, LeaseEpoch: 1, Actor: "w1", CreatedAt: now},
	}
	got, err := Fold(events)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateReviewPending {
		t.Fatalf("state=%s want review_pending", got.State)
	}
	if got.LeaseEpoch != 1 {
		t.Fatalf("epoch=%d want 1 (monotonic, not reset)", got.LeaseEpoch)
	}
	if got.LeaseID != "" || got.BoundIdentity != "" {
		t.Fatalf("lease columns must be cleared after result, got id=%q ident=%q", got.LeaseID, got.BoundIdentity)
	}
	if got.BaseSHA != "base1" || got.Priority != 7 || got.Kind != job.KindBuild {
		t.Fatalf("seed facts lost: %+v", got)
	}
	if got.JobSeq != 5 {
		t.Fatalf("job_seq cursor=%d want 5", got.JobSeq)
	}
}

func TestFoldReleaseIncrementsAttempts(t *testing.T) {
	events := []Event{
		{JobID: "J", JobSeq: 1, Kind: KindJobCreated, ToState: job.StateReady, Actor: "system", Payload: Payload{Kind: job.KindBuild, Flow: "build"}},
		{JobID: "J", JobSeq: 2, Kind: KindLeaseClaimed, ToState: job.StateLeased, LeaseEpoch: 1, Payload: Payload{LeaseID: "L"}},
		{JobID: "J", JobSeq: 3, Kind: KindLeaseReleased, ToState: job.StateReady, LeaseEpoch: 1, Payload: Payload{AttemptsDelta: 1}},
	}
	got, _ := Fold(events)
	if got.State != job.StateReady {
		t.Fatalf("state=%s want ready", got.State)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts=%d want 1", got.Attempts)
	}
	if got.LeaseID != "" {
		t.Fatal("lease id must be cleared on release")
	}
}
