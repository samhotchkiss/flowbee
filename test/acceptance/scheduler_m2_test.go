// M2 acceptance: the scheduler core DONE-WHEN, proven end-to-end over the real
// HTTP surface against a real SQLite store, no GitHub / LLM.
//
//	(1) hand-seeded A->B->C DAG: B stays blocked until A is done;
//	(2) an aged low-priority job is offered before a high-priority newcomer;
//	(3) a worker lacking a required capability never wins, and no_eligible_worker
//	    fires after a timeout (driven by the real durable-timer poller);
//	(4) the run is reconstructable by replaying job_events.
package acceptance

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/alarm"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// TestDAGGatesUntilDoneE2E proves (1) and (4): A->B->C over HTTP leasing.
func TestDAGGatesUntilDoneE2E(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(10_000, 0))
	srv := newM2Server(st, clk)
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	seedM2(t, st, "A", nil, nil, time.Unix(1000, 0))
	seedM2(t, st, "B", []string{"A"}, nil, time.Unix(1000, 0))
	seedM2(t, st, "C", []string{"B"}, nil, time.Unix(1000, 0))

	c := registerWorker(t, ctx, ts.URL, "alice", "codex")

	// Only A is leasable; B and C are blocked. The first lease yields A.
	g, ok, err := c.Lease(ctx, "alice", "codex", "")
	if err != nil || !ok {
		t.Fatalf("expected lease of A, ok=%v err=%v", ok, err)
	}
	if g.JobID != "A" {
		t.Fatalf("first lease=%s want A (B/C blocked)", g.JobID)
	}

	// finish A: result -> review_pending, then complete -> done (unblocks B).
	if _, _, err := c.Result(ctx, "A", g.LeaseEpoch, "", map[string]any{"kind": "patch"}); err != nil {
		t.Fatalf("result A: %v", err)
	}
	// B still blocked until A is *done*, not merely review_pending.
	if jb, _ := st.GetJob(ctx, "B"); jb.State != job.StateBlocked {
		t.Fatalf("B=%s want blocked while A only review_pending", jb.State)
	}
	if _, err := st.CompleteJob(ctx, store.CompleteParams{JobID: "A", Now: clk.Now()}); err != nil {
		t.Fatalf("complete A: %v", err)
	}

	// now B is leasable.
	g2, ok2, err := c.Lease(ctx, "alice", "codex", "")
	if err != nil || !ok2 {
		t.Fatalf("expected lease of B after A done, ok=%v err=%v", ok2, err)
	}
	if g2.JobID != "B" {
		t.Fatalf("lease=%s want B", g2.JobID)
	}

	// (4) run reconstructable by replaying job_events.
	for _, id := range []string{"A", "B", "C"} {
		evs, _ := st.LoadEvents(ctx, id)
		if len(evs) == 0 {
			t.Fatalf("%s has no events", id)
		}
		folded, _ := ledger.Fold(evs)
		proj, _ := st.GetJob(ctx, id)
		if folded.State != proj.State || folded.JobSeq != proj.JobSeq {
			t.Fatalf("%s Fold(%s,seq%d) != projection(%s,seq%d)", id, folded.State, folded.JobSeq, proj.State, proj.JobSeq)
		}
	}
}

// TestAgedLowPriorityOfferedFirstE2E proves (2): an aged low-priority job is
// offered before a high-priority newcomer, over the HTTP lease.
func TestAgedLowPriorityOfferedFirstE2E(t *testing.T) {
	st := testutil.NewStore(t)
	// "now" is 2 hours past the aged job's enqueue so aging dominates priority.
	clk := clock.NewFake(time.Unix(1000, 0).Add(2 * time.Hour))
	srv := newM2Server(st, clk)
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	// low-priority job enqueued long ago.
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "aged-low", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, Priority: 0, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	// high-priority newcomer enqueued just now.
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "fresh-high", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, Priority: 5, Now: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	c := registerWorker(t, ctx, ts.URL, "alice", "codex")
	g, ok, err := c.Lease(ctx, "alice", "codex", "")
	if err != nil || !ok {
		t.Fatalf("expected a lease, ok=%v err=%v", ok, err)
	}
	if g.JobID != "aged-low" {
		t.Fatalf("lease=%s want aged-low (aging beats fresh high-prio)", g.JobID)
	}
}

// TestNoEligibleWorkerAlarmE2E proves (3): a worker lacking the required
// capability never wins; the no_eligible_worker alarm fires after the window via
// the real durable-timer poller.
func TestNoEligibleWorkerAlarmE2E(t *testing.T) {
	st := testutil.NewStore(t)
	st.NoEligibleWorkerDelay = 30 * time.Second
	clk := clock.NewFake(time.Unix(1000, 0))
	srv := newM2Server(st, clk)
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	// a job that requires a codex worker.
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "needs-codex", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, RequiredCapabilities: []string{"role:eng_worker", "model_family:codex"},
		Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}

	// an opus worker tries: it must NOT win (204), and the job stays ready.
	c := registerWorker(t, ctx, ts.URL, "opus-worker", "opus")
	if _, ok, err := c.Lease(ctx, "opus-worker", "opus", ""); err != nil || ok {
		t.Fatalf("opus worker must not win codex job: ok=%v err=%v", ok, err)
	}
	if j, _ := st.GetJob(ctx, "needs-codex"); j.State != job.StateReady {
		t.Fatalf("job=%s want still ready", j.State)
	}

	// drive the poller: advance the clock past the alarm window and tick once.
	poller := alarm.New(st, clk, time.Hour, srv.Broker())
	clk.Advance(31 * time.Second)
	poller.Tick(ctx)

	if ok, _ := st.AlarmFired(ctx, "needs-codex", store.TimerNoEligibleWorker); !ok {
		t.Fatal("no_eligible_worker alarm did not fire after timeout")
	}
	// and it is in the ledger (reconstructable).
	evs, _ := st.LoadEvents(ctx, "needs-codex")
	found := false
	for _, e := range evs {
		if e.Kind == ledger.KindNoEligibleWorker {
			found = true
		}
	}
	if !found {
		t.Fatal("no_eligible_worker event missing from ledger")
	}
}

// ── helpers ──

func newM2Server(st *store.Store, clk clock.Clock) *api.Server {
	return api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 500 * time.Millisecond,
		LeaseTTLS: 300, HeartbeatIntervalS: 30,
	}, "m2")
}

func seedM2(t *testing.T, st *store.Store, id string, blockedBy, req []string, now time.Time) {
	t.Helper()
	if _, err := st.SeedJob(context.Background(), store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "base-" + id, BlockedBy: blockedBy, RequiredCapabilities: req, Now: now,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func registerWorker(t *testing.T, ctx context.Context, url, identity, family string) *client.Client {
	t.Helper()
	c := client.New(url)
	if _, err := c.Register(ctx, client.Registration{
		Identity: identity, Host: "test",
		Capabilities: []string{"role:eng_worker", "model_family:" + family},
	}); err != nil {
		t.Fatalf("register %s: %v", identity, err)
	}
	return c
}
