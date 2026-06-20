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

// TestFoldCarriesHeadOnReArm locks the fix for a projection!=Fold divergence on head_sha:
// the head-establishing events (KindResultAccepted, KindRebased, KindConflictResolved) set
// head_sha via a direct store UPDATE, so the fold must reproduce it from the event payload —
// otherwise a rebuild-from-ledger blanks (result/rebase) or strands (resolve) the head, and
// reconcile's flowbeePlaced guard (which reads head_sha to tell our own integrated head from
// an external push) misclassifies the next sweep and spuriously supersedes the review.
func TestFoldCarriesHeadOnReArm(t *testing.T) {
	now := time.Unix(100, 0)
	base := []Event{
		{JobID: "H1", JobSeq: 1, Kind: KindJobCreated, ToState: job.StateReady, CreatedAt: now,
			Payload: Payload{Kind: job.KindBuild, Role: job.RoleEngWorker, RequiredCapabilities: []string{"role:eng_worker"}}},
		{JobID: "H1", JobSeq: 2, Kind: KindLeaseClaimed, ToState: job.StateLeased, LeaseEpoch: 1, CreatedAt: now},
		{JobID: "H1", JobSeq: 3, Kind: KindResultAccepted, ToState: job.StateReviewPending, LeaseEpoch: 1, CreatedAt: now},
	}

	// a clean rebase re-arms review at the integrated head -> the fold must carry it (the
	// rebase UPDATE sets head_sha = newSHA; the fold previously blanked it to "").
	reb := append(append([]Event{}, base...),
		Event{JobID: "H1", JobSeq: 4, Kind: KindRebased, ToState: job.StateReviewPending, LeaseEpoch: 2, CreatedAt: now,
			Payload: Payload{BaseSHA: "newbase", HeadSHA: "rebasedhead"}})
	if j, err := Fold(reb); err != nil {
		t.Fatal(err)
	} else if j.HeadSHA != "rebasedhead" {
		t.Errorf("rebase fold head=%q want rebasedhead (a blank head breaks the flowbeePlaced guard)", j.HeadSHA)
	}

	// a conflict resolution re-arms at the resolver's pushed head -> the fold must carry it
	// (the resolve UPDATE sets head_sha = PushedSHA; the fold previously left it stale).
	res := append(append([]Event{}, base...),
		Event{JobID: "H1", JobSeq: 4, Kind: KindConflictResolved, ToState: job.StateReviewPending, LeaseEpoch: 1, CreatedAt: now,
			Payload: Payload{BaseSHA: "newbase", HeadSHA: "resolvedhead"}})
	if j, err := Fold(res); err != nil {
		t.Fatal(err)
	} else if j.HeadSHA != "resolvedhead" {
		t.Errorf("resolve fold head=%q want resolvedhead", j.HeadSHA)
	}

	// an empty resolved head keeps the prior head — mirrors the store's
	// COALESCE(NULLIF(PushedSHA,''), head_sha).
	keep := append(append([]Event{}, reb...),
		Event{JobID: "H1", JobSeq: 5, Kind: KindConflictResolved, ToState: job.StateReviewPending, LeaseEpoch: 2, CreatedAt: now,
			Payload: Payload{HeadSHA: ""}})
	if j, err := Fold(keep); err != nil {
		t.Fatal(err)
	} else if j.HeadSHA != "rebasedhead" {
		t.Errorf("empty resolved head must keep the prior head; got %q want rebasedhead", j.HeadSHA)
	}

	// the BUILD RESULT itself carries the worker's pushed head -> the fold must reproduce it
	// (the result UPDATE sets head_sha = COALESCE(NULLIF(PushedSHA,''), head_sha)). Without it
	// a rebuild blanks head_sha for any job sitting in review_pending/code_review/mergeable
	// before a verdict mints the SHA, and the next sweep supersedes a good built+CI'd job.
	built := []Event{
		{JobID: "H2", JobSeq: 1, Kind: KindJobCreated, ToState: job.StateReady, CreatedAt: now,
			Payload: Payload{Kind: job.KindBuild, Role: job.RoleEngWorker, RequiredCapabilities: []string{"role:eng_worker"}}},
		{JobID: "H2", JobSeq: 2, Kind: KindLeaseClaimed, ToState: job.StateLeased, LeaseEpoch: 1, CreatedAt: now},
		{JobID: "H2", JobSeq: 3, Kind: KindResultAccepted, ToState: job.StateReviewPending, LeaseEpoch: 1, CreatedAt: now,
			Payload: Payload{HeadSHA: "builthead"}},
	}
	if j, err := Fold(built); err != nil {
		t.Fatal(err)
	} else if j.HeadSHA != "builthead" {
		t.Errorf("result-accepted fold head=%q want builthead (a blank head breaks the flowbeePlaced guard)", j.HeadSHA)
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
