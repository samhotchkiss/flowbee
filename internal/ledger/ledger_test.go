package ledger

import (
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
)

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
