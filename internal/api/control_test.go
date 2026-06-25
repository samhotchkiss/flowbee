package api_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func ctrlServer(t *testing.T) (*store.Store, *client.Client, *clock.Fake) {
	t.Helper()
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(1000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: time.Minute, LongPollWait: 50 * time.Millisecond,
		LeaseTTLS: 60, HeartbeatIntervalS: 60,
	}, "ctrl")
	ts := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(ts.Close)
	c := client.New(ts.URL)
	if _, err := c.Register(context.Background(), client.Registration{
		WorkerID: "w", Identity: "w", Host: "h",
		Capabilities: []string{"role:eng_worker", "model_family:codex"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return st, c, clk
}

func seedReady(t *testing.T, st *store.Store, id, repo string, now time.Time) {
	t.Helper()
	if _, err := st.SeedJob(context.Background(), store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, BaseSHA: "base", Repo: repo, Now: now,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// TestControlGlobalPause: a client tells the dispatcher "pause everything" → the lease
// endpoint hands out NO work even though a ready job exists; resume re-opens it.
func TestControlGlobalPause(t *testing.T) {
	ctx := context.Background()
	st, c, clk := ctrlServer(t)
	seedReady(t, st, "j", "", clk.Now())

	if err := c.Pause(ctx, ""); err != nil { // "" => global, "pause everything"
		t.Fatalf("pause: %v", err)
	}
	if _, ok, err := c.Lease(ctx, "w", "codex", ""); err != nil || ok {
		t.Fatalf("paused dispatch must hand out no work (ok=%v err=%v)", ok, err)
	}
	if err := c.Resume(ctx, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if g, ok, err := c.Lease(ctx, "w", "codex", ""); err != nil || !ok || g.JobID != "j" {
		t.Fatalf("resumed dispatch must lease the ready job (ok=%v job=%s err=%v)", ok, g.JobID, err)
	}
}

// TestLeaseGrantCarriesCIFailures: a build job that carries recorded CI failures from a
// prior attempt (last_ci_failures) leases as a REBUILD with those failing-check names in
// its context — even when bounces==0, because a manual `requeue` zeroes the bounce counter
// while preserving the failure memory. This is what lets a requeued build target the exact
// gate it failed instead of rebuilding blind.
func TestLeaseGrantCarriesCIFailures(t *testing.T) {
	ctx := context.Background()
	st, c, clk := ctrlServer(t)
	seedReady(t, st, "j", "", clk.Now())
	// recorded failing checks, bounces still 0 (the requeue-then-grant case).
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET last_ci_failures='Architecture and guardrail lints'||char(10)||'golangci-lint' WHERE id='j'`); err != nil {
		t.Fatal(err)
	}

	g, ok, err := c.Lease(ctx, "w", "codex", "")
	if err != nil || !ok || g.Context == nil {
		t.Fatalf("lease: ok=%v err=%v ctx=%v", ok, err, g.Context)
	}
	if !g.Context.Rebuild {
		t.Fatalf("a build with recorded CI failures must lease as a rebuild")
	}
	for _, want := range []string{"Architecture and guardrail lints", "golangci-lint"} {
		if !strings.Contains(g.Context.CIFailures, want) {
			t.Fatalf("grant CIFailures=%q missing %q", g.Context.CIFailures, want)
		}
	}
}

// TestReportRebaseConflictDivertsToResolver: a builder that holds a live lease and finds
// its branch patch won't apply onto the granted base reports the conflict (with its branch
// diff) through the client; the control plane diverts the job to the conflict_resolver path
// at the conflicting base, storing the diff as the job's patch for the resolver.
func TestReportRebaseConflictDivertsToResolver(t *testing.T) {
	ctx := context.Background()
	st, c, clk := ctrlServer(t)
	seedReady(t, st, "j", "", clk.Now())

	g, ok, err := c.Lease(ctx, "w", "codex", "")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	const diff = "diff --git a/x b/x\n+conflicting change"
	if status, err := c.ReportRebaseConflict(ctx, "j", g.LeaseEpoch, "newmain", diff); err != nil || status != 200 {
		t.Fatalf("report rebase conflict: status=%d err=%v", status, err)
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateResolvingConflict || j.Role != job.RoleConflictResolver {
		t.Fatalf("job not diverted: state=%s role=%s", j.State, j.Role)
	}
	if j.BaseSHA != "newmain" {
		t.Fatalf("base_sha=%s, want newmain", j.BaseSHA)
	}
	if d, _ := st.JobPatchDiff(ctx, "j"); d != diff {
		t.Fatalf("patch_diff=%q, want the reported branch diff", d)
	}

	// a stale epoch (lost the lease) is fenced with 409 and leaves the job alone.
	if status, _ := c.ReportRebaseConflict(ctx, "j", 999, "x", "d"); status != 409 {
		t.Fatalf("stale-epoch report status=%d, want 409", status)
	}
}

// TestLeaseGatedWhenAccountRateLimited: a worker whose agent login (FLOWBEE_ACCOUNT) is
// rate-limited gets NO work (the F6 per-account ceiling), so dispatch rolls over to boxes
// on accounts that aren't maxed; once the account clears, work flows again.
func TestLeaseGatedWhenAccountRateLimited(t *testing.T) {
	ctx := context.Background()
	st, c, clk := ctrlServer(t)
	seedReady(t, st, "j", "", clk.Now())
	st.UpsertAccounts(ctx, []store.AccountSpec{
		{AccountID: "codex:s@swh.me", ModelFamily: "codex", CeilingPct: 90},
	}, clk.Now())
	t.Setenv("FLOWBEE_ACCOUNT", "codex:s@swh.me") // the client sends this as account_id

	// account is fine -> work is offered.
	if g, ok, err := c.Lease(ctx, "w", "codex", ""); err != nil || !ok || g.JobID != "j" {
		t.Fatalf("healthy account should lease the job (ok=%v job=%s err=%v)", ok, g.JobID, err)
	}
	// re-arm the job to ready for the next claim, then rate-limit the account.
	if _, err := st.RequeueJob(ctx, "j", true, clk.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordUsage(ctx, []capacity.UsageReport{
		{AccountID: "codex:s@swh.me", ModelFamily: "codex", UsagePct: 100, RateLimited: true},
	}, clk.Now()); err != nil {
		t.Fatal(err)
	}
	// gated -> no work, even though a ready job exists.
	if _, ok, err := c.Lease(ctx, "w", "codex", ""); err != nil || ok {
		t.Fatalf("a rate-limited account must get NO work (ok=%v err=%v)", ok, err)
	}
	// clear the account -> work flows again.
	if _, err := st.RecordUsage(ctx, []capacity.UsageReport{
		{AccountID: "codex:s@swh.me", ModelFamily: "codex", UsagePct: 0, RateLimited: false},
	}, clk.Now()); err != nil {
		t.Fatal(err)
	}
	if g, ok, err := c.Lease(ctx, "w", "codex", ""); err != nil || !ok || g.JobID != "j" {
		t.Fatalf("cleared account should lease again (ok=%v job=%s err=%v)", ok, g.JobID, err)
	}
}

func TestLeaseDryRunDoesNotClaim(t *testing.T) {
	ctx := context.Background()
	st, c, clk := ctrlServer(t)
	seedReady(t, st, "j", "", clk.Now())

	grant, ok, err := c.LeaseDryRun(ctx, "w", "codex", "")
	if err != nil || !ok {
		t.Fatalf("dry-run lease: ok=%v err=%v", ok, err)
	}
	if grant.JobID != "j" || !grant.DryRun {
		t.Fatalf("dry-run grant = job %q dry=%v, want j/true", grant.JobID, grant.DryRun)
	}
	if grant.LeaseID != "dry-run" || grant.LeaseEpoch != 1 {
		t.Fatalf("dry-run lease fields id=%q epoch=%d, want dry-run/1", grant.LeaseID, grant.LeaseEpoch)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateReady || j.LeaseEpoch != 0 || j.LeaseID != "" {
		t.Fatalf("dry-run mutated job: state=%s epoch=%d lease=%q", j.State, j.LeaseEpoch, j.LeaseID)
	}

	realGrant, ok, err := c.Lease(ctx, "w", "codex", "")
	if err != nil || !ok || realGrant.JobID != "j" || realGrant.DryRun {
		t.Fatalf("real lease after dry-run: ok=%v job=%q dry=%v err=%v", ok, realGrant.JobID, realGrant.DryRun, err)
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateLeased || j.LeaseEpoch != 1 || j.LeaseID == "" {
		t.Fatalf("real lease did not claim job: state=%s epoch=%d lease=%q", j.State, j.LeaseEpoch, j.LeaseID)
	}
}

// TestControlPerRepoPause: parking ONE repo drops its jobs from the lease queue while every
// other repo keeps flowing — the "pause all russ jobs" case, leaving flowbee untouched.
func TestControlPerRepoPause(t *testing.T) {
	ctx := context.Background()
	st, c, clk := ctrlServer(t)
	for _, id := range []string{"keep", "park"} {
		if err := st.RegisterRepo(ctx, store.Repo{ID: id, Owner: "o", Repo: id, DefaultBranch: "main", Active: true}); err != nil {
			t.Fatalf("register repo %s: %v", id, err)
		}
	}
	seedReady(t, st, "jpark", "park", clk.Now())
	seedReady(t, st, "jkeep", "keep", clk.Now())

	// park only the "park" repo.
	if err := c.Pause(ctx, "park"); err != nil {
		t.Fatalf("pause repo: %v", err)
	}
	// the worker is offered "keep"'s job, NEVER the parked one.
	g, ok, err := c.Lease(ctx, "w", "codex", "")
	if err != nil || !ok {
		t.Fatalf("a non-parked repo must still flow (ok=%v err=%v)", ok, err)
	}
	if g.JobID != "jkeep" {
		t.Fatalf("leased %s, want jkeep — a parked repo's job must not be offered", g.JobID)
	}
	// the parked job is still present, just withheld.
	if j, _ := st.GetJob(ctx, "jpark"); j.State != job.StateReady {
		t.Fatalf("parked job should stay ready (withheld), got %s", j.State)
	}
}
