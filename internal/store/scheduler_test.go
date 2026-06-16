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

func seedDep(t *testing.T, st *store.Store, id string, blockedBy []string, req []string, now time.Time) job.Job {
	t.Helper()
	j, err := st.SeedJob(context.Background(), store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "base-" + id, BlockedBy: blockedBy, RequiredCapabilities: req, Now: now,
	})
	if err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	return j
}

// TestDAGBlockedUntilDone: A->B->C. B stays blocked until A is done; C until B.
func TestDAGBlockedUntilDone(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	a := seedDep(t, st, "A", nil, nil, time.Unix(1000, 0))
	b := seedDep(t, st, "B", []string{"A"}, nil, time.Unix(1000, 0))
	c := seedDep(t, st, "C", []string{"B"}, nil, time.Unix(1000, 0))

	if a.State != job.StateReady {
		t.Fatalf("A seeded %s want ready", a.State)
	}
	if b.State != job.StateBlocked || c.State != job.StateBlocked {
		t.Fatalf("B=%s C=%s want both blocked", b.State, c.State)
	}

	// drive A to review_pending then done.
	ls, err := claim(st, "A", "w1")
	if err != nil {
		t.Fatalf("claim A: %v", err)
	}
	if _, err := st.Result(ctx, store.ResultParams{JobID: "A", Epoch: ls.Epoch, Now: time.Unix(2000, 0)}); err != nil {
		t.Fatalf("result A: %v", err)
	}
	// B must still be blocked (A is only review_pending, not done).
	if jb, _ := st.GetJob(ctx, "B"); jb.State != job.StateBlocked {
		t.Fatalf("B=%s want still blocked before A done", jb.State)
	}

	unblocked, err := st.CompleteJob(ctx, store.CompleteParams{JobID: "A", Now: time.Unix(3000, 0)})
	if err != nil {
		t.Fatalf("complete A: %v", err)
	}
	if len(unblocked) != 1 || unblocked[0] != "B" {
		t.Fatalf("completing A should unblock B, got %v", unblocked)
	}
	if jb, _ := st.GetJob(ctx, "B"); jb.State != job.StateReady {
		t.Fatalf("B=%s want ready after A done", jb.State)
	}
	// C still blocked (B not done yet).
	if jc, _ := st.GetJob(ctx, "C"); jc.State != job.StateBlocked {
		t.Fatalf("C=%s want still blocked", jc.State)
	}

	// finish B -> C unblocks.
	lb, _ := claim(st, "B", "w2")
	st.Result(ctx, store.ResultParams{JobID: "B", Epoch: lb.Epoch, Now: time.Unix(4000, 0)})
	unblocked2, _ := st.CompleteJob(ctx, store.CompleteParams{JobID: "B", Now: time.Unix(5000, 0)})
	if len(unblocked2) != 1 || unblocked2[0] != "C" {
		t.Fatalf("completing B should unblock C, got %v", unblocked2)
	}
	if jc, _ := st.GetJob(ctx, "C"); jc.State != job.StateReady {
		t.Fatalf("C=%s want ready", jc.State)
	}

	// the whole run is reconstructable by replaying job_events.
	for _, id := range []string{"A", "B", "C"} {
		evs, _ := st.LoadEvents(ctx, id)
		folded, _ := ledger.Fold(evs)
		proj, _ := st.GetJob(ctx, id)
		if folded.State != proj.State {
			t.Fatalf("%s Fold state=%s != projection %s", id, folded.State, proj.State)
		}
	}
}

// TestMultiDepWaitsForAll: a job blocked by two predecessors clears only when
// BOTH are done.
func TestMultiDepWaitsForAll(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedDep(t, st, "P1", nil, nil, time.Unix(1000, 0))
	seedDep(t, st, "P2", nil, nil, time.Unix(1000, 0))
	seedDep(t, st, "Q", []string{"P1", "P2"}, nil, time.Unix(1000, 0))

	finish := func(id string, base int) {
		ls, _ := claim(st, id, "w-"+id)
		st.Result(ctx, store.ResultParams{JobID: id, Epoch: ls.Epoch, Now: time.Unix(int64(base), 0)})
		st.CompleteJob(ctx, store.CompleteParams{JobID: id, Now: time.Unix(int64(base+1), 0)})
	}

	finish("P1", 2000)
	if jq, _ := st.GetJob(ctx, "Q"); jq.State != job.StateBlocked {
		t.Fatalf("Q=%s want still blocked after only P1 done", jq.State)
	}
	finish("P2", 3000)
	if jq, _ := st.GetJob(ctx, "Q"); jq.State != job.StateReady {
		t.Fatalf("Q=%s want ready after both done", jq.State)
	}
}

// TestCapabilityClaimRejected: a worker lacking a required attested capability
// cannot win the row (ErrLostRace); a compliant worker can.
func TestCapabilityClaimRejected(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedDep(t, st, "needs", nil, []string{"role:eng_worker", "model_family:codex"}, time.Unix(1000, 0))

	// opus worker lacks model_family:codex -> lost race, job stays ready.
	_, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "needs", LeaseID: "l1", Identity: "opus-w", ModelFamily: "opus",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker", "model_family:opus"},
		TTL: time.Minute, Now: time.Unix(1100, 0),
	})
	if err == nil {
		t.Fatal("opus worker must NOT win a codex-required job")
	}
	if j, _ := st.GetJob(ctx, "needs"); j.State != job.StateReady {
		t.Fatalf("job state=%s want still ready", j.State)
	}

	// codex worker wins.
	ls, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "needs", LeaseID: "l2", Identity: "codex-w", ModelFamily: "codex",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker", "model_family:codex"},
		TTL: time.Minute, Now: time.Unix(1200, 0),
	})
	if err != nil || ls == nil {
		t.Fatalf("codex worker should win: %v", err)
	}
}

// TestNoEligibleWorkerAlarmFires: an armed alarm fires when the job is still
// `ready` (no compliant worker claimed it) and the window elapsed.
func TestNoEligibleWorkerAlarmFires(t *testing.T) {
	st := testutil.NewStore(t)
	st.NoEligibleWorkerDelay = 30 * time.Second
	ctx := context.Background()

	seedDep(t, st, "lonely", nil, []string{"role:eng_worker", "model_family:codex"}, time.Unix(1000, 0))

	// before the window: not due.
	due, _ := st.DueTimers(ctx, time.Unix(1010, 0))
	if len(due) != 0 {
		t.Fatalf("timer should not be due yet, got %d", len(due))
	}
	// after the window: due, and it fires (job still ready at epoch 0).
	due, _ = st.DueTimers(ctx, time.Unix(1040, 0))
	if len(due) != 1 {
		t.Fatalf("timer should be due, got %d", len(due))
	}
	fired, err := st.FireNoEligibleWorker(ctx, due[0], time.Unix(1040, 0))
	if err != nil || !fired {
		t.Fatalf("alarm should fire: fired=%v err=%v", fired, err)
	}
	if ok, _ := st.AlarmFired(ctx, "lonely", store.TimerNoEligibleWorker); !ok {
		t.Fatal("alarm record missing")
	}
	// the alarm is in the ledger (reconstructable).
	evs, _ := st.LoadEvents(ctx, "lonely")
	found := false
	for _, e := range evs {
		if e.Kind == ledger.KindNoEligibleWorker {
			found = true
		}
	}
	if !found {
		t.Fatal("no_eligible_worker event not in ledger")
	}
}

// TestNoEligibleWorkerAlarmStaleNoop: once the job is claimed (epoch bumps), the
// armed timer is a no-op (epoch guard).
func TestNoEligibleWorkerAlarmStaleNoop(t *testing.T) {
	st := testutil.NewStore(t)
	st.NoEligibleWorkerDelay = 30 * time.Second
	ctx := context.Background()

	seedDep(t, st, "j", nil, nil, time.Unix(1000, 0))
	// a compliant worker claims it (epoch 0 -> 1).
	if _, err := claim(st, "j", "w1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	due, _ := st.DueTimers(ctx, time.Unix(1040, 0))
	if len(due) != 1 {
		t.Fatalf("expected the armed timer due, got %d", len(due))
	}
	fired, err := st.FireNoEligibleWorker(ctx, due[0], time.Unix(1040, 0))
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if fired {
		t.Fatal("a claimed job's alarm must NOT fire (stale epoch / not ready)")
	}
	if ok, _ := st.AlarmFired(ctx, "j", store.TimerNoEligibleWorker); ok {
		t.Fatal("no alarm should be recorded for a leased job")
	}
}
