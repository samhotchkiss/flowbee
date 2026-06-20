// M8 acceptance: Liveness MVP (Rung-3 + Rung-4 + minimal Rung-2 + two fast-paths +
// the two-rung kill, I-13), proven end-to-end over the real HTTP worker surface
// against a real SQLite store + a fake clock + fakeGitHub (BUILD.md §6.4 — no real
// creds, no network).
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - a forever-heartbeating no-net-diff worker is killed ONLY when Rung-2 + Rung-3
//     agree (NOT when Rung-2 abstains);
//   - past the absolute cap -> unilateral revoke;
//   - a partitioned worker's healed-link result is 409'd (fencing handles the zombie);
//   - an exited agent is fast-pathed to failed;
//   - a job past the Rung-4 governor ceiling sticks in needs_human;
//   - a healthy long E2E with a CI running-transition is NOT killed.
package acceptance

import (
	"context"
	"database/sql"
	"net/http"
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

// m8Env wires the control plane (private API) + the single durable-timer poller
// with liveness driving, all against a fake clock so deadlines are deterministic.
type m8Env struct {
	st      *store.Store
	clk     *clock.Fake
	srv     *api.Server
	poller  *alarm.Poller
	cfg     store.LivenessConfig
	private *httptest.Server
}

func newM8Env(t *testing.T) *m8Env {
	t.Helper()
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 30 * time.Minute, LongPollWait: 500 * time.Millisecond,
		LeaseTTLS: 1800, HeartbeatIntervalS: 60,
	}, "m8")
	cfg := store.LivenessConfig{
		PhaseBudget:                   10 * time.Minute, // soft deadline
		AbsoluteCap:                   60 * time.Minute, // un-gameable Rung-3 floor
		Rung2Window:                   10 * time.Minute,
		GovernorCeiling:               2, // anti-thrash ceiling (distinct from max_attempts)
		CircuitBreakerAbstainFraction: 0.9,
	}
	// the poller reads reconciled facts (domain_b_facts) for Rung-2 via DBFactSource.
	poller := alarm.New(st, clk, time.Millisecond, srv.Broker()).
		WithLiveness(cfg, store.DBFactSource{DB: st.DB}, srv.Broker())
	private := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(private.Close)
	return &m8Env{st: st, clk: clk, srv: srv, poller: poller, cfg: cfg, private: private}
}

// leaseBuild seeds a ready build job, leases it as an eng_worker, and arms the
// liveness deadline timers for the new lease — exactly what the runtime does on a
// claim. Returns the client + the lease grant.
func (e *m8Env) leaseBuild(t *testing.T, ctx context.Context, jobID, identity, family string) (*client.Client, client.LeaseGrant) {
	t.Helper()
	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base-sha-0",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed %s: %v", jobID, err)
	}
	c := client.New(e.private.URL)
	if _, err := c.Register(ctx, client.Registration{
		WorkerID: "wk-" + identity, Identity: identity, Host: "t",
		Capabilities: []string{"role:eng_worker", "model_family:" + family},
	}); err != nil {
		t.Fatalf("register %s: %v", identity, err)
	}
	g, ok, err := c.Lease(ctx, identity, family, "")
	if err != nil || !ok || g.JobID != jobID {
		t.Fatalf("lease %s ok=%v err=%v", jobID, ok, err)
	}
	if err := e.st.ArmLeaseLivenessTimers(ctx, jobID, g.LeaseEpoch, e.clk.Now(), e.cfg); err != nil {
		t.Fatalf("arm liveness timers: %v", err)
	}
	return c, g
}

// TestM8_ClaimArmsSoftDeadlineViaServer proves the PRODUCTION wiring: a lease claimed
// through the API server (with SetLiveness configured, exactly as serve.go does at
// startup) arms the soft phase-deadline + the durable deadline timers ITSELF — no manual
// ArmLeaseLivenessTimers. The bug this guards: the claim set only lease_deadline, never
// phase_deadline_at, so SoftCrossed never fired and the §10.2 soft-deadline early-
// escalation rung was silently inert in production (the unit tests passed only because
// the harness armed the timers by hand). Note: this test does NOT call leaseBuild — it
// claims raw, so the ONLY thing that can arm the soft deadline is the server.
func TestM8_ClaimArmsSoftDeadlineViaServer(t *testing.T) {
	e := newM8Env(t)
	e.srv.SetLiveness(e.cfg) // what serve.go does once at startup
	ctx := context.Background()
	jobID := "armed-by-claim"

	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base-sha-0",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := client.New(e.private.URL)
	if _, err := c.Register(ctx, client.Registration{
		WorkerID: "wk-zoe", Identity: "zoe", Host: "t",
		Capabilities: []string{"role:eng_worker", "model_family:codex"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	g, ok, err := c.Lease(ctx, "zoe", "codex", "")
	if err != nil || !ok || g.JobID != jobID {
		t.Fatalf("lease ok=%v err=%v", ok, err)
	}

	// the claim itself must have armed the soft deadline (not just the absolute cap) —
	// the inert-rung bug was phase_deadline_at staying NULL after a real claim.
	var phaseAt sql.NullString
	if err := e.st.DB.QueryRowContext(ctx,
		`SELECT phase_deadline_at FROM jobs WHERE id = ?`, jobID).Scan(&phaseAt); err != nil {
		t.Fatalf("read phase_deadline_at: %v", err)
	}
	if !phaseAt.Valid || phaseAt.String == "" {
		t.Fatal("claim through the API server must arm phase_deadline_at (the §10.2 soft deadline); it was NULL — the soft rung would be inert")
	}

	// and the full ladder now fires from that production-armed deadline: a worker
	// reporting a Rung-1 `spinning` suspicion PAST the soft deadline is killed (soft
	// deadline + Rung-1 suspicion, §10.3) — driven by the durable phase_deadline timer
	// the claim armed, with nobody arming it by hand.
	if _, st, herr := c.HeartbeatWith(ctx, jobID, g.LeaseEpoch, client.HeartbeatObs{
		AgentHealth: "ok", Rung1Class: "spinning",
	}); herr != nil || st != http.StatusOK {
		t.Fatalf("heartbeat st=%d err=%v", st, herr)
	}
	e.clk.Advance(11 * time.Minute) // past the 10-min soft deadline, before the 60-min cap
	e.poller.Tick(ctx)              // the durable phase_deadline timer fires -> FireLeaseDeadline -> EvaluateLiveness
	j, _ := e.st.GetJob(ctx, jobID)
	if j.State != job.StateReady {
		t.Fatalf("soft deadline + Rung-1 suspicion must kill via the production-armed timer (revoke -> ready); state=%s", j.State)
	}
	if j.LeaseEpoch != g.LeaseEpoch+1 {
		t.Fatalf("a kill must bump the epoch; %d -> %d", g.LeaseEpoch, j.LeaseEpoch)
	}
}

// ── TestM8_NoNetDiffKilledOnlyWhenTwoRungsAgree ───────────────────────────────
// A forever-heartbeating worker whose build never gains net diff is killed ONLY
// when Rung-2 (stalled) AND Rung-3 (soft deadline) agree — NOT while Rung-2
// abstains (no first ref push) even with the worker reporting `spinning`.
func TestM8_NoNetDiffKilledOnlyWhenTwoRungsAgree(t *testing.T) {
	e := newM8Env(t)
	ctx := context.Background()
	jobID := "stall-1"
	c, g := e.leaseBuild(t, ctx, jobID, "amy", "codex")

	// the worker forever-heartbeats reporting healthy activity (Rung-0 ok, Rung-1
	// working). Crucially it reports NO worker-side suspicion, so the ONLY path to a
	// kill is the un-gameable rungs (Rung-2 + Rung-3) — exactly the DONE-WHEN: a
	// no-net-diff worker is killed only when Rung-2 AND Rung-3 agree, never when
	// Rung-2 abstains (a worker-reported `spinning` + soft deadline would itself be a
	// valid two-rung kill per §10.3; we exclude it to isolate the Rung-2 condition).
	beat := func() {
		dir, st, err := c.HeartbeatWith(ctx, jobID, g.LeaseEpoch, client.HeartbeatObs{
			AgentHealth: "ok", Rung1Class: "working",
		})
		if err != nil || st != http.StatusOK {
			t.Fatalf("heartbeat st=%d err=%v", st, err)
		}
		if dir == "cancel" {
			t.Fatalf("a forever-heartbeating worker must not be cancelled while Rung-2 abstains")
		}
	}
	beat()

	// advance PAST the soft deadline, but Rung-2 still ABSTAINS (no reconciled head
	// SHA yet — the build never pushed its first ref). The soft-deadline timer fires,
	// but with abstain there is no second rung: NO kill (§10.3 row 4).
	e.clk.Advance(11 * time.Minute) // past the 10-min phase budget, before the 60-min cap
	e.poller.Tick(ctx)              // fires the phase_deadline timer
	e.poller.Rung2Tick(ctx)         // Rung-2 abstains (no facts)
	beat()                          // still alive, still continue
	if j, _ := e.st.GetJob(ctx, jobID); j.State != job.StateLeased {
		t.Fatalf("soft deadline + Rung-2 abstain must NOT kill; state=%s", j.State)
	}
	if v, _ := e.st.Rung2VerdictFor(ctx, jobID); v != "abstain" {
		t.Fatalf("Rung-2 must abstain with no reconciled SHA, got %s", v)
	}

	// now the build pushes a ref and reconcile observes a head SHA: Rung-2 opens its
	// window at that SHA (converging). The SHA then never moves (no net diff) while
	// the worker keeps claiming activity.
	if err := e.st.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
		PRExists: true, PRNumber: 1, HeadSHA: "head-A", BaseSHA: "base-sha-0", CIGreen: false,
	}); err != nil {
		t.Fatalf("seed facts: %v", err)
	}
	e.poller.Rung2Tick(ctx) // opens the window at head-A -> converging
	if v, _ := e.st.Rung2VerdictFor(ctx, jobID); v != "converging" {
		t.Fatalf("first reconciled SHA must read converging, got %s", v)
	}
	beat()

	// the SHA stays head-A for longer than the Rung-2 window: now Rung-2 -> stalled.
	// With the soft deadline ALREADY crossed, the two un-gameable rungs (Rung-2
	// stalled + Rung-3 soft) agree -> KILL (revoke -> ready, epoch++).
	e.clk.Advance(11 * time.Minute) // window aged past 10 min; still before the 60-min cap
	e.poller.Rung2Tick(ctx)         // Rung-2 -> stalled, then the eval pass kills
	j, _ := e.st.GetJob(ctx, jobID)
	if j.State != job.StateReady {
		t.Fatalf("Rung-2 stalled + Rung-3 soft must kill (revoke -> ready); state=%s", j.State)
	}
	if j.LeaseEpoch != g.LeaseEpoch+1 {
		t.Fatalf("a kill must bump the epoch (the zombie's fence): %d -> %d", g.LeaseEpoch, j.LeaseEpoch)
	}
	if j.StallRevocations != 1 {
		t.Fatalf("the Rung-4 governor counter must increment, got %d", j.StallRevocations)
	}

	// the killed worker's next fenced call carries the now-stale epoch -> 409.
	if _, st, _ := c.HeartbeatWith(ctx, jobID, g.LeaseEpoch, client.HeartbeatObs{}); st != http.StatusConflict {
		t.Fatalf("the revoked worker's heartbeat must 409, got %d", st)
	}

	// the ledger records the revoke; Fold reproduces the projection (determinism).
	evs, _ := e.st.LoadEvents(ctx, jobID)
	folded, _ := ledger.Fold(evs)
	if folded.State != j.State || folded.LeaseEpoch != j.LeaseEpoch || folded.StallRevocations != j.StallRevocations {
		t.Fatalf("Fold != projection: fold={%s e%d sr%d} proj={%s e%d sr%d}",
			folded.State, folded.LeaseEpoch, folded.StallRevocations,
			j.State, j.LeaseEpoch, j.StallRevocations)
	}
}

// ── TestM8_AbsoluteCapUnilateralRevoke ────────────────────────────────────────
// Past the absolute lease cap, a perfectly-heartbeating worker with Rung-2
// abstaining the whole time is revoked UNILATERALLY (the lone Rung-3 kill).
func TestM8_AbsoluteCapUnilateralRevoke(t *testing.T) {
	e := newM8Env(t)
	ctx := context.Background()
	jobID := "cap-1"
	c, g := e.leaseBuild(t, ctx, jobID, "ben", "opus")

	// heartbeat healthily right up to (but before) the cap: no kill.
	e.clk.Advance(59 * time.Minute)
	e.poller.Tick(ctx)      // the phase timer fired long ago, but abstain => no kill
	e.poller.Rung2Tick(ctx) // abstains (no facts ever)
	if _, st, _ := c.HeartbeatWith(ctx, jobID, g.LeaseEpoch, client.HeartbeatObs{AgentHealth: "ok"}); st != http.StatusOK {
		t.Fatalf("before the cap a healthy worker survives, got %d", st)
	}
	if j, _ := e.st.GetJob(ctx, jobID); j.State != job.StateLeased {
		t.Fatalf("before the cap the job must still hold its lease, got %s", j.State)
	}

	// cross the absolute cap: the lease_deadline timer fires -> UNILATERAL revoke,
	// even though Rung-2 abstained the entire time (no second rung needed).
	e.clk.Advance(2 * time.Minute) // now 61 min > 60-min cap
	e.poller.Tick(ctx)             // fires lease_deadline_check
	j, _ := e.st.GetJob(ctx, jobID)
	if j.State != job.StateReady {
		t.Fatalf("past the absolute cap the lease must be revoked (-> ready), got %s", j.State)
	}
	if j.LeaseEpoch != g.LeaseEpoch+1 {
		t.Fatalf("the cap revoke must bump the epoch, %d -> %d", g.LeaseEpoch, j.LeaseEpoch)
	}
	// the over-cap worker is fenced.
	if _, st, _ := c.HeartbeatWith(ctx, jobID, g.LeaseEpoch, client.HeartbeatObs{}); st != http.StatusConflict {
		t.Fatalf("the over-cap worker must be fenced (409), got %d", st)
	}
}

// ── TestM8_PartitionedWorkerHealedResult409 ───────────────────────────────────
// A worker that goes silent (partition) past the cap is revoked + re-dispatched;
// when its link heals and it POSTs a full result, the now-stale epoch is rejected
// 409 — exactly-once acknowledgement even though it may have executed a full build
// (partition != stall; fencing handles the zombie, §10.5).
func TestM8_PartitionedWorkerHealedResult409(t *testing.T) {
	e := newM8Env(t)
	ctx := context.Background()
	jobID := "part-1"
	c, g := e.leaseBuild(t, ctx, jobID, "carl", "codex")

	// the worker partitions: it simply stops heartbeating. Flowbee's clock crosses
	// the absolute cap and reclaims the lease (epoch bump). Re-dispatch to ready.
	e.clk.Advance(61 * time.Minute)
	e.poller.Tick(ctx)
	if j, _ := e.st.GetJob(ctx, jobID); j.State != job.StateReady || j.LeaseEpoch != g.LeaseEpoch+1 {
		t.Fatalf("a partitioned worker past the cap must be reclaimed; state=%s epoch=%d", j.State, j.LeaseEpoch)
	}

	// the link heals: the partitioned worker (which may have executed a FULL build)
	// POSTs its result carrying the OLD epoch -> 409. The build is acknowledged
	// exactly once; the zombie's work is quarantined by the epoch fence.
	_, st, err := c.Result(ctx, jobID, g.LeaseEpoch, "healed-1",
		map[string]any{"kind": "patch", "base_sha": "base-sha-0", "pushed_ref": "refs/flowbee/part-1/epoch-1"})
	if err != nil {
		t.Fatalf("healed result: %v", err)
	}
	if st != http.StatusConflict {
		t.Fatalf("the healed-link result must be 409'd (stale epoch), got %d", st)
	}
	// the job is untouched by the zombie: still ready at the bumped epoch.
	if j, _ := e.st.GetJob(ctx, jobID); j.State != job.StateReady {
		t.Fatalf("the zombie result must not advance the job, got %s", j.State)
	}
}

// ── TestM8_AgentExitedFastPathToFailed ────────────────────────────────────────
// The locally-provable agent_exited_zombie fast-path (§10.6): the worker reports
// its agent PID died; Flowbee fast-paths straight to `failed` (not a kill — the
// agent is already dead), bumping the epoch.
func TestM8_AgentExitedFastPathToFailed(t *testing.T) {
	e := newM8Env(t)
	ctx := context.Background()
	jobID := "exit-1"
	c, g := e.leaseBuild(t, ctx, jobID, "dot", "opus")

	dir, st, err := c.HeartbeatWith(ctx, jobID, g.LeaseEpoch, client.HeartbeatObs{
		AgentHealth: "ok", AgentExited: true,
	})
	if err != nil || st != http.StatusOK {
		t.Fatalf("fast-path heartbeat st=%d err=%v", st, err)
	}
	if dir != "cancel" {
		t.Fatalf("agent_exited must return a cancel directive, got %q", dir)
	}
	j, _ := e.st.GetJob(ctx, jobID)
	if j.State != job.StateFailed {
		t.Fatalf("an exited agent must be fast-pathed to failed, got %s", j.State)
	}
	if j.LeaseEpoch != g.LeaseEpoch+1 {
		t.Fatalf("the fast-path must fence the zombie (epoch bump), %d -> %d", g.LeaseEpoch, j.LeaseEpoch)
	}
	// the dead agent's zombie successor (if any) is fenced.
	if _, st, _ := c.HeartbeatWith(ctx, jobID, g.LeaseEpoch, client.HeartbeatObs{}); st != http.StatusConflict {
		t.Fatalf("a post-exit call must be fenced 409, got %d", st)
	}
	// Fold reproduces failed (determinism).
	evs, _ := e.st.LoadEvents(ctx, jobID)
	if folded, _ := ledger.Fold(evs); folded.State != job.StateFailed {
		t.Fatalf("Fold != projection for the fast-path: %s", folded.State)
	}
}

// ── TestM8_GovernorCeilingSticksInNeedsHuman ──────────────────────────────────
// A repeatedly killed-and-resumed job, once it crosses the Rung-4 governor ceiling,
// sticks in needs_human (anti-thrash) rather than re-dispatching forever (§10.7).
func TestM8_GovernorCeilingSticksInNeedsHuman(t *testing.T) {
	e := newM8Env(t)
	ctx := context.Background()
	jobID := "thrash-1"

	// drive the job through the governor ceiling (2) of absolute-cap kills. Each
	// cycle: lease -> arm -> cross the cap -> revoke. The first kill re-dispatches
	// (-> ready); the kill that REACHES the ceiling sticks in needs_human.
	var lastState job.State
	for cycle := 1; cycle <= e.cfg.GovernorCeiling; cycle++ {
		c, g := e.leaseOrRelease(t, ctx, jobID, cycle == 1, "ed", "codex")
		_ = c
		e.clk.Advance(61 * time.Minute)
		e.poller.Tick(ctx) // fires the cap timer for THIS epoch
		j, _ := e.st.GetJob(ctx, jobID)
		lastState = j.State
		if cycle < e.cfg.GovernorCeiling {
			if j.State != job.StateReady {
				t.Fatalf("cycle %d: a kill below the ceiling must re-dispatch (-> ready), got %s", cycle, j.State)
			}
		}
		_ = g
	}
	if lastState != job.StateNeedsHuman {
		t.Fatalf("crossing the governor ceiling must stick in needs_human, got %s", lastState)
	}
	j, _ := e.st.GetJob(ctx, jobID)
	if j.StallRevocations < e.cfg.GovernorCeiling {
		t.Fatalf("stall_revocations must reach the ceiling, got %d", j.StallRevocations)
	}
	// it is NOT re-armed: a further eval pass leaves it in needs_human (non-terminal,
	// awaiting a deliberate human resume).
	e.poller.Rung2Tick(ctx)
	if j2, _ := e.st.GetJob(ctx, jobID); j2.State != job.StateNeedsHuman {
		t.Fatalf("needs_human must hold against thrash, got %s", j2.State)
	}
}

// leaseOrRelease leases the job fresh on the first cycle, or (on a re-dispatch) waits
// for the job to be `ready` and re-leases it, arming the new lease's timers.
func (e *m8Env) leaseOrRelease(t *testing.T, ctx context.Context, jobID string, first bool, identity, family string) (*client.Client, client.LeaseGrant) {
	t.Helper()
	if first {
		return e.leaseBuild(t, ctx, jobID, identity, family)
	}
	c := client.New(e.private.URL)
	// re-register (idempotent) and re-lease the re-dispatched job.
	if _, err := c.Register(ctx, client.Registration{
		WorkerID: "wk-" + identity, Identity: identity, Host: "t",
		Capabilities: []string{"role:eng_worker", "model_family:" + family},
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	g, ok, err := c.Lease(ctx, identity, family, "")
	if err != nil || !ok || g.JobID != jobID {
		t.Fatalf("re-lease ok=%v err=%v", ok, err)
	}
	if err := e.st.ArmLeaseLivenessTimers(ctx, jobID, g.LeaseEpoch, e.clk.Now(), e.cfg); err != nil {
		t.Fatalf("re-arm timers: %v", err)
	}
	return c, g
}

// ── TestM8_HealthyLongE2EWithCITransitionNotKilled ────────────────────────────
// A healthy long-running build (a 40-min E2E suite) whose CI records a `running`
// transition is NOT killed: the CI-running tolerance extends Rung-2's window so
// "no new diff during the suite" is EXPECTED, not stalled — Rung-2 never votes
// stalled, so even past the soft deadline the two-rung rule is never satisfied
// (Guardrail A, §10.4).
func TestM8_HealthyLongE2ENotKilled(t *testing.T) {
	e := newM8Env(t)
	ctx := context.Background()
	jobID := "e2e-1"
	c, g := e.leaseBuild(t, ctx, jobID, "fay", "codex")

	// the build pushed its ref; reconcile observes head-A and CI is RUNNING (the E2E
	// suite started). This is hard proof the agent's output reached the outside world.
	if err := e.st.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
		PRExists: true, PRNumber: 7, HeadSHA: "head-A", BaseSHA: "base-sha-0", CIGreen: false,
	}); err != nil {
		t.Fatalf("seed facts: %v", err)
	}
	if err := e.st.MarkCIRunning(ctx, jobID, true, e.clk.Now()); err != nil {
		t.Fatalf("mark ci running: %v", err)
	}

	// 40 minutes pass with the SHA unchanged (the suite runs) — well past the 10-min
	// soft deadline. The worker heartbeats `frozen` (it's waiting on CI). Drive the
	// poller repeatedly across the window.
	for min := 0; min < 40; min += 5 {
		e.clk.Advance(5 * time.Minute)
		e.poller.Tick(ctx)      // the soft-deadline timer fires somewhere in here
		e.poller.Rung2Tick(ctx) // CI running keeps Rung-2 at converging, NOT stalled
		dir, st, err := c.HeartbeatWith(ctx, jobID, g.LeaseEpoch, client.HeartbeatObs{
			AgentHealth: "ok", Rung1Class: "frozen",
		})
		if err != nil || st != http.StatusOK {
			t.Fatalf("min %d heartbeat st=%d err=%v", min, st, err)
		}
		if dir == "cancel" {
			t.Fatalf("min %d: a healthy CI-running E2E must NOT be cancelled", min)
		}
		if v, _ := e.st.Rung2VerdictFor(ctx, jobID); v == "stalled" {
			t.Fatalf("min %d: CI-running tolerance must keep Rung-2 off `stalled`, got %s", min, v)
		}
	}
	// still building, still under the absolute cap (40 < 60 min): not killed.
	j, _ := e.st.GetJob(ctx, jobID)
	if j.State != job.StateLeased {
		t.Fatalf("a healthy CI-running 40-min E2E must survive (lease held), got %s", j.State)
	}
	if j.LeaseEpoch != g.LeaseEpoch {
		t.Fatalf("the lease must not have been revoked (epoch unchanged): %d", j.LeaseEpoch)
	}

	// the suite finishes: CI goes green, the build advances normally (no kill ever).
	if err := e.st.MarkCIRunning(ctx, jobID, false, e.clk.Now()); err != nil {
		t.Fatalf("clear ci running: %v", err)
	}
	if _, st, err := c.Result(ctx, jobID, g.LeaseEpoch, "e2e-done",
		map[string]any{"kind": "patch", "base_sha": "base-sha-0", "pushed_ref": "refs/flowbee/e2e-1/epoch-1"}); err != nil || st != http.StatusOK {
		t.Fatalf("a survived E2E must complete normally, st=%d err=%v", st, err)
	}
	if j, _ := e.st.GetJob(ctx, jobID); j.State != job.StateReviewPending {
		t.Fatalf("the completed E2E must land review_pending, got %s", j.State)
	}
}
