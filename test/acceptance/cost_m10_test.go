// M10 acceptance: Cost metering + ceilings (I-15) + the unified escalation
// chokepoint (DESIGN §6.7, §12.6.5), proven end-to-end over the real HTTP worker
// surface against a real SQLite store + a fake clock + an in-memory fakeGitHub
// (BUILD.md §6.4 — no real creds, no network) + the serialized project-OUT sender.
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - a job crossing its token/$ ceiling is escalated: a LIVE `cancel` directive on
//     the heartbeat that trips it + a flowbee:over-budget label rendered via
//     project-OUT, and the job sits in needs_human (never silently overspends);
//   - a per-flow rollup answers "what did this feature cost across spec+build+
//     review?" by summing every job of the feature;
//   - the needs_human chokepoint view shows jobs from ALL FOUR triggers
//     (attempts, bounces, cost, stall).
package acceptance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/alarm"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/project"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

type m10Env struct {
	st      *store.Store
	fake    *gh.Fake
	clk     *clock.Fake
	srv     *api.Server
	sender  *project.Sender
	poller  *alarm.Poller
	cfg     store.LivenessConfig
	private *httptest.Server
}

func newM10Env(t *testing.T) *m10Env {
	t.Helper()
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 30 * time.Minute, LongPollWait: 500 * time.Millisecond,
		LeaseTTLS: 1800, HeartbeatIntervalS: 60,
	}, "m10")
	fake := gh.NewFake()
	sender := project.New(st, fake, clk, srv.Broker())
	cfg := store.LivenessConfig{
		PhaseBudget: 10 * time.Minute, AbsoluteCap: 60 * time.Minute,
		Rung2Window: 10 * time.Minute, GovernorCeiling: 2,
		CircuitBreakerAbstainFraction: 0.9,
	}
	poller := alarm.New(st, clk, time.Millisecond, srv.Broker()).
		WithLiveness(cfg, store.DBFactSource{DB: st.DB}, srv.Broker())
	private := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(private.Close)
	return &m10Env{st: st, fake: fake, clk: clk, srv: srv, sender: sender, poller: poller, cfg: cfg, private: private}
}

func (e *m10Env) drain(t *testing.T, ctx context.Context) {
	t.Helper()
	for {
		n, err := e.sender.DrainOnce(ctx)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if n == 0 {
			return
		}
	}
}

// leaseBuildWithCeiling seeds a ready build job carrying a $ ceiling + a flow_id,
// leases it as an eng_worker, and arms the liveness timers (what the runtime does
// on a claim). Returns the client + lease grant.
func (e *m10Env) leaseBuildWithCeiling(t *testing.T, ctx context.Context, jobID, flowID, identity, family string, ceilingMicroUSD int64) (*client.Client, client.LeaseGrant) {
	t.Helper()
	c := ceilingMicroUSD
	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base-sha-0", FlowID: flowID,
		CostCeilingMicroUSD:  &c,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed %s: %v", jobID, err)
	}
	cl := client.New(e.private.URL)
	if _, err := cl.Register(ctx, client.Registration{
		WorkerID: "wk-" + identity, Identity: identity, Host: "t",
		Capabilities: []string{"role:eng_worker", "model_family:" + family},
	}); err != nil {
		t.Fatalf("register %s: %v", identity, err)
	}
	g, ok, err := cl.Lease(ctx, identity, family, "")
	if err != nil || !ok || g.JobID != jobID {
		t.Fatalf("lease %s ok=%v err=%v", jobID, ok, err)
	}
	if err := e.st.ArmLeaseLivenessTimers(ctx, jobID, g.LeaseEpoch, e.clk.Now(), e.cfg); err != nil {
		t.Fatalf("arm timers: %v", err)
	}
	return cl, g
}

// ── DONE-WHEN 1: a job crossing its ceiling is escalated (live cancel + label) ──
func TestM10_CeilingCrossingEscalates(t *testing.T) {
	e := newM10Env(t)
	ctx := context.Background()
	jobID := "cost-1"
	// $10.00 ceiling = 10_000_000 micro-USD.
	_, g := e.leaseBuildWithCeiling(t, ctx, jobID, "feat-x", "amy", "codex", 10_000_000)

	// the job has an open PR (Flowbee opened it; here we stamp it so the over-budget
	// label has a number to render onto). head_sha keys the (job,action,sha) dedupe.
	if err := e.st.StampPRNumber(ctx, jobID, 7, "head-cost", "base-sha-0", e.clk.Now()); err != nil {
		t.Fatalf("stamp pr: %v", err)
	}

	// first heartbeat reports $4.00 of cost — well under the ceiling -> continue.
	dir, st, err := e.heartbeatCost(ctx, g, jobID, 41000, 8000, 4_000_000)
	if err != nil || st != http.StatusOK {
		t.Fatalf("under-ceiling heartbeat st=%d err=%v", st, err)
	}
	if dir != "continue" {
		t.Fatalf("under the ceiling expected continue, got %q", dir)
	}
	if j, _ := e.st.GetJob(ctx, jobID); !job.HasActiveLease(j.State) {
		t.Fatalf("under the ceiling the job must keep its active lease, got %s", j.State)
	}
	if j, _ := e.st.GetJob(ctx, jobID); j.CostMicroUSD != 4_000_000 {
		t.Fatalf("meter must accumulate, got %d", j.CostMicroUSD)
	}

	// the next heartbeat reports another $7.00 -> $11.00 total, OVER the $10 ceiling.
	// I-15: the job escalates to needs_human, the worker gets a LIVE `cancel`, the
	// epoch bumps (the worker is fenced), and over_budget is marked.
	dir, st, err = e.heartbeatCost(ctx, g, jobID, 90000, 21000, 7_000_000)
	if err != nil || st != http.StatusOK {
		t.Fatalf("ceiling-crossing heartbeat st=%d err=%v", st, err)
	}
	if dir != "cancel" {
		t.Fatalf("crossing the ceiling must return a LIVE cancel directive, got %q", dir)
	}
	j, _ := e.st.GetJob(ctx, jobID)
	if j.State != job.StateNeedsHuman {
		t.Fatalf("over budget must escalate to needs_human, got %s", j.State)
	}
	if !j.OverBudget {
		t.Fatal("over_budget must be marked (I-15)")
	}
	if j.EscalationReason != string(job.EscalationCost) {
		t.Fatalf("escalation_reason=%q want cost", j.EscalationReason)
	}
	if j.LeaseEpoch != g.LeaseEpoch+1 {
		t.Fatalf("escalation must bump the epoch (fence the worker): %d -> %d", g.LeaseEpoch, j.LeaseEpoch)
	}
	if j.CostMicroUSD != 11_000_000 {
		t.Fatalf("meter must include the tripping report, got %d", j.CostMicroUSD)
	}

	// the fenced worker's next call (stale epoch) is 409'd — never silently overspends.
	if _, st, _ := e.heartbeatCost(ctx, g, jobID, 1, 1, 1_000_000); st != http.StatusConflict {
		t.Fatalf("the over-budget worker's stale heartbeat must 409, got %d", st)
	}

	// project-OUT: drain the outbox and assert the flowbee:over-budget label landed
	// on the PR (the §12.6.5 / §8.3 rendering).
	e.drain(t, ctx)
	labels := e.fake.Labels(7)
	if !contains(labels, "flowbee:over-budget") {
		t.Fatalf("PR must carry flowbee:over-budget, got %v", labels)
	}

	// the ledger records the escalation; Fold reproduces the projection (determinism).
	evs, _ := e.st.LoadEvents(ctx, jobID)
	folded, _ := ledger.Fold(evs)
	if folded.State != j.State || folded.LeaseEpoch != j.LeaseEpoch ||
		folded.CostMicroUSD != j.CostMicroUSD || folded.OverBudget != j.OverBudget {
		t.Fatalf("Fold != projection: fold={%s e%d $%d ob%v} proj={%s e%d $%d ob%v}",
			folded.State, folded.LeaseEpoch, folded.CostMicroUSD, folded.OverBudget,
			j.State, j.LeaseEpoch, j.CostMicroUSD, j.OverBudget)
	}
}

// ── DONE-WHEN 2: per-flow rollup across spec+build+review ──────────────────────
func TestM10_PerFlowRollup(t *testing.T) {
	e := newM10Env(t)
	ctx := context.Background()
	flowID := "feat-rollup"

	// three jobs of the SAME feature flow (a spec_review, an eng_worker build, a
	// code_review), each metering cost under its ceiling. No ceiling crossing here —
	// just accumulation, so the rollup answers the end-to-end cost question.
	type seedSpec struct {
		id, flow, stage, identity, family string
		role                              job.Role
		caps                              []string
		in, out, usd                      int64
	}
	specs := []seedSpec{
		{"feat-spec", "spec", "review", "speccer", "haiku", job.RoleEngWorker, []string{"role:eng_worker"}, 44_000, 9_000, 700_000},
		{"feat-build", "build", "build", "builder", "codex", job.RoleEngWorker, []string{"role:eng_worker"}, 412_000, 88_000, 6_100_000},
		{"feat-review", "build", "build", "reviewer", "opus", job.RoleEngWorker, []string{"role:eng_worker"}, 120_000, 14_000, 1_900_000},
	}
	for _, s := range specs {
		c := int64(20_000_000) // generous per-job ceiling, never tripped
		if _, err := e.st.SeedJob(ctx, store.SeedParams{
			ID: s.id, Kind: job.KindBuild, Flow: s.flow, Stage: s.stage,
			Role: s.role, BaseSHA: "b", FlowID: flowID, CostCeilingMicroUSD: &c,
			RequiredCapabilities: s.caps, Now: e.clk.Now(),
		}); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
		cl := client.New(e.private.URL)
		if _, err := cl.Register(ctx, client.Registration{
			WorkerID: "wk-" + s.identity, Identity: s.identity, Host: "t",
			Capabilities: []string{"role:eng_worker", "model_family:" + s.family},
		}); err != nil {
			t.Fatalf("register %s: %v", s.identity, err)
		}
		g, ok, err := cl.Lease(ctx, s.identity, s.family, "")
		if err != nil || !ok || g.JobID != s.id {
			t.Fatalf("lease %s ok=%v err=%v", s.id, ok, err)
		}
		if dir, st, err := e.heartbeatCost(ctx, g, s.id, s.in, s.out, s.usd); err != nil || st != http.StatusOK || dir != "continue" {
			t.Fatalf("meter %s dir=%q st=%d err=%v", s.id, dir, st, err)
		}
	}

	roll, err := e.st.FlowCostRollup(ctx, flowID)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(roll.Jobs) != 3 {
		t.Fatalf("rollup must cover all three jobs of the feature, got %d", len(roll.Jobs))
	}
	wantUSD := int64(700_000 + 6_100_000 + 1_900_000)
	if roll.TotalMicroUSD != wantUSD {
		t.Fatalf("feature total = %d micro-USD, want %d (spec+build+review)", roll.TotalMicroUSD, wantUSD)
	}
	if roll.TotalTokensIn != 44_000+412_000+120_000 {
		t.Fatalf("feature tokens_in total wrong, got %d", roll.TotalTokensIn)
	}
	if roll.TotalTokensOut != 9_000+88_000+14_000 {
		t.Fatalf("feature tokens_out total wrong, got %d", roll.TotalTokensOut)
	}
}

// ── DONE-WHEN 3: needs_human view shows jobs from all four triggers ────────────
func TestM10_NeedsHumanViewAllFourTriggers(t *testing.T) {
	e := newM10Env(t)
	ctx := context.Background()

	// (a) COST: a job crossing its ceiling -> needs_human (the real M10 path).
	costJob := "nh-cost"
	_, gc := e.leaseBuildWithCeiling(t, ctx, costJob, "f-cost", "cathy", "codex", 5_000_000)
	if dir, st, err := e.heartbeatCost(ctx, gc, costJob, 1, 1, 6_000_000); err != nil || st != http.StatusOK || dir != "cancel" {
		t.Fatalf("cost escalation dir=%q st=%d err=%v", dir, st, err)
	}

	// (b) STALL: a two-rung kill at the Rung-4 governor ceiling -> needs_human.
	stallJob := "nh-stall"
	e.driveStallToNeedsHuman(t, ctx, stallJob)

	// (c) BOUNCES: a code_review job bounced past max_bounces -> needs_human.
	bounceJob := "nh-bounce"
	e.driveBouncesToNeedsHuman(t, ctx, bounceJob)

	// (d) ATTEMPTS: a job whose attempts are exhausted at the absolute cap ->
	// needs_human (the §6.7 attempts ceiling).
	attemptsJob := "nh-attempts"
	e.driveAttemptsToNeedsHuman(t, ctx, attemptsJob)

	view, err := e.st.NeedsHumanView(ctx)
	if err != nil {
		t.Fatalf("needs_human view: %v", err)
	}
	reason := map[string]string{}
	for _, r := range view {
		reason[r.JobID] = r.Reason
	}
	cases := []struct{ id, want string }{
		{costJob, string(job.EscalationCost)},
		{stallJob, string(job.EscalationStall)},
		{bounceJob, string(job.EscalationBounces)},
		{attemptsJob, string(job.EscalationAttempts)},
	}
	for _, c := range cases {
		if got, ok := reason[c.id]; !ok {
			t.Fatalf("needs_human view missing %s (trigger %s)", c.id, c.want)
		} else if got != c.want {
			t.Fatalf("%s shows trigger %q, want %q", c.id, got, c.want)
		}
	}
	if len(view) != 4 {
		t.Fatalf("the chokepoint must show exactly the four escalated jobs, got %d", len(view))
	}
}

// heartbeatCost sends a fenced heartbeat carrying a cost delta and returns the
// directive + HTTP status.
func (e *m10Env) heartbeatCost(ctx context.Context, g client.LeaseGrant, jobID string, in, out, usd int64) (string, int, error) {
	cl := client.New(e.private.URL)
	return cl.HeartbeatWith(ctx, jobID, g.LeaseEpoch, client.HeartbeatObs{
		AgentHealth: "ok", Rung1Class: "working",
		TokensInDelta: in, TokensOutDelta: out, MicroUSDDelta: usd,
	})
}

// driveStallToNeedsHuman drives a build job through repeated two-rung stall kills
// until the Rung-4 governor ceiling holds it in needs_human (§10.7).
func (e *m10Env) driveStallToNeedsHuman(t *testing.T, ctx context.Context, jobID string) {
	t.Helper()
	// GovernorCeiling is 2: seed once, then drive two two-rung stall kills
	// (re-dispatch in between); the 2nd holds the job in needs_human. A huge ceiling
	// keeps cost out of this path.
	huge := int64(1_000_000_000)
	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base-sha-0", FlowID: "f-stall",
		CostCeilingMicroUSD:  &huge,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed stall job: %v", err)
	}
	for round := 0; round < 2; round++ {
		j, _ := e.st.GetJob(ctx, jobID)
		if j.State != job.StateReady {
			t.Fatalf("stall round %d: job=%s want ready", round, j.State)
		}
		ident := stallWorker(round)
		cl := client.New(e.private.URL)
		if _, err := cl.Register(ctx, client.Registration{
			WorkerID: "wk-" + ident, Identity: ident, Host: "t",
			Capabilities: []string{"role:eng_worker", "model_family:codex"},
		}); err != nil {
			t.Fatalf("register %s: %v", ident, err)
		}
		g, ok, err := cl.Lease(ctx, ident, "codex", "")
		if err != nil || !ok || g.JobID != jobID {
			t.Fatalf("stall lease round %d ok=%v err=%v", round, ok, err)
		}
		if err := e.st.ArmLeaseLivenessTimers(ctx, jobID, g.LeaseEpoch, e.clk.Now(), e.cfg); err != nil {
			t.Fatalf("arm timers: %v", err)
		}
		// reconcile a head SHA so Rung-2 opens its window, then never move it.
		if err := e.st.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
			PRExists: true, PRNumber: 30 + round, HeadSHA: "head-stall", BaseSHA: "base-sha-0",
		}); err != nil {
			t.Fatalf("seed facts: %v", err)
		}
		e.poller.Rung2Tick(ctx)         // window opens at head-stall -> converging
		e.clk.Advance(11 * time.Minute) // past the soft deadline AND the Rung-2 window
		e.poller.Tick(ctx)              // fires phase deadline (Rung-3 soft)
		e.poller.Rung2Tick(ctx)         // Rung-2 -> stalled; eval pass kills (two-rung)
	}
	j, _ := e.st.GetJob(ctx, jobID)
	if j.State != job.StateNeedsHuman {
		t.Fatalf("after the governor ceiling the stall job must stick in needs_human, got %s (sr=%d)", j.State, j.StallRevocations)
	}
}

// driveBouncesToNeedsHuman drives a code_review job through max_bounces
// changes_requested verdicts until it escalates to needs_human (§6.7).
func (e *m10Env) driveBouncesToNeedsHuman(t *testing.T, ctx context.Context, jobID string) {
	t.Helper()
	reviewer, rg := driveToCodeReview(t, ctx, e.st, e.private.URL, jobID)
	// pin max_bounces=3 (below the per-reviewer cap of 6) so the TOTAL-bounce backstop
	// is the trigger here — this asserts the "bounces" escalation reason specifically,
	// independent of the shipped default (now the higher backstop).
	if _, err := e.st.DB.ExecContext(ctx, `UPDATE jobs SET max_bounces=3 WHERE id=?`, jobID); err != nil {
		t.Fatalf("pin max_bounces: %v", err)
	}
	// Bounce three times: the 3rd reaches the ceiling -> needs_human. The gate bounces
	// on changes_requested regardless of facts.
	epoch := rg.LeaseEpoch
	for i := 0; i < 3; i++ {
		resp, code, err := reviewer.Review(ctx, jobID, epoch, "bounce-"+itoa(i), "changes_requested", "", "")
		if err != nil || code != http.StatusOK {
			t.Fatalf("bounce %d code=%d err=%v", i, code, err)
		}
		if i < 2 {
			if resp.JobState != string(job.StateReady) {
				t.Fatalf("bounce %d -> state=%s want ready", i, resp.JobState)
			}
			// rebuild + re-review for the next bounce round.
			rebuildToReviewPending(t, ctx, e.st, e.private.URL, jobID, i+1)
			rl := rereview(t, ctx, e.st, e.private.URL, jobID)
			epoch = rl.epoch
			reviewer = rl.cl
		} else if resp.JobState != string(job.StateNeedsHuman) {
			t.Fatalf("final bounce -> state=%s want needs_human", resp.JobState)
		}
	}
	if j, _ := e.st.GetJob(ctx, jobID); j.State != job.StateNeedsHuman || j.Bounces < j.MaxBounces {
		t.Fatalf("bounce job must be needs_human with bounces>=max, got state=%s bounces=%d/%d", j.State, j.Bounces, j.MaxBounces)
	}
}

// driveAttemptsToNeedsHuman drives a build job to attempts-exhaustion via repeated
// lease releases, then a final absolute-cap kill that escalates (attempts ceiling).
func (e *m10Env) driveAttemptsToNeedsHuman(t *testing.T, ctx context.Context, jobID string) {
	t.Helper()
	huge := int64(1_000_000_000)
	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base-sha-0", FlowID: "f-attempts",
		CostCeilingMicroUSD:  &huge,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed attempts job: %v", err)
	}
	// max_attempts defaults to 5. Release the lease 4 times (attempts -> 4), each
	// returning the job to ready, then on the 5th lease cross the absolute cap: the
	// cap kill sees attempts+1 >= max_attempts and escalates to needs_human.
	ident := "attempts-worker"
	cl := client.New(e.private.URL)
	if _, err := cl.Register(ctx, client.Registration{
		WorkerID: "wk-" + ident, Identity: ident, Host: "t",
		Capabilities: []string{"role:eng_worker", "model_family:codex"},
	}); err != nil {
		t.Fatalf("register %s: %v", ident, err)
	}
	for i := 0; i < 4; i++ {
		g, ok, err := cl.Lease(ctx, ident, "codex", "")
		if err != nil || !ok || g.JobID != jobID {
			t.Fatalf("attempts lease %d ok=%v err=%v", i, ok, err)
		}
		if st, err := cl.Release(ctx, jobID, g.LeaseEpoch); err != nil || st != http.StatusOK {
			t.Fatalf("attempts release %d st=%d err=%v", i, st, err)
		}
	}
	if j, _ := e.st.GetJob(ctx, jobID); j.Attempts != 4 {
		t.Fatalf("after 4 releases attempts=%d want 4", j.Attempts)
	}
	// 5th lease, then cross the absolute cap -> attempts-exhausted escalation.
	g, ok, err := cl.Lease(ctx, ident, "codex", "")
	if err != nil || !ok || g.JobID != jobID {
		t.Fatalf("final attempts lease ok=%v err=%v", ok, err)
	}
	if err := e.st.ArmLeaseLivenessTimers(ctx, jobID, g.LeaseEpoch, e.clk.Now(), e.cfg); err != nil {
		t.Fatalf("arm timers: %v", err)
	}
	e.clk.Advance(61 * time.Minute) // past the 60-min absolute cap
	e.poller.Tick(ctx)              // fires lease_deadline_check -> unilateral revoke + escalate
	j, _ := e.st.GetJob(ctx, jobID)
	if j.State != job.StateNeedsHuman {
		t.Fatalf("attempts-exhausted cap kill must escalate to needs_human, got %s (attempts=%d/%d)", j.State, j.Attempts, j.MaxAttempts)
	}
}

func stallWorker(round int) string { return "stall-worker-" + itoa(round) }

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
