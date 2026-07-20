package multirepo_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/multirepo"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

type failingSweepClient struct {
	*gh.Fake
	err      error
	attempts int
}

func (c *failingSweepClient) BoardSweep(context.Context) (gh.BoardSnapshot, error) {
	c.attempts++
	return gh.BoardSnapshot{}, c.err
}

// seedReadyBuild seeds a ready build job scoped to a repo with a priority and an
// eng_worker capability requirement (so a shared eng_worker pool can win it).
func seedReadyBuild(t *testing.T, st *store.Store, id, repo string, priority int, now time.Time) {
	t.Helper()
	if _, err := st.SeedJob(context.Background(), store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "base-" + id, Priority: priority, Repo: repo,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// TestMultiRepoControlPlane is the F9 acceptance test (build-list F9 DONE-WHEN):
// TWO repos managed by ONE control plane (fakeGitHub x2); a job from each routes to
// the SHARED worker pool; reconcile/project run PER repo; the scheduler prioritizes
// ACROSS repos.
func TestMultiRepoControlPlane(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(10_000, 0)
	clk := clock.NewFake(now)

	// ── one control plane, a SET of repos ──
	if err := st.RegisterRepo(ctx, store.Repo{ID: "core", Owner: "acme", Repo: "core", DefaultBranch: "main", Active: true}); err != nil {
		t.Fatalf("register core: %v", err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "web", Owner: "acme", Repo: "web", DefaultBranch: "trunk", Active: true}); err != nil {
		t.Fatalf("register web: %v", err)
	}

	// per-repo fakeGitHub x2 (workers hold NO GitHub creds; Flowbee owns the writes).
	fakes := map[string]*gh.Fake{
		"core": gh.NewFake(),
		"web":  gh.NewFake(),
	}
	factory := func(r store.Repo) (gh.Client, gh.Writer, error) {
		f := fakes[r.ID]
		return f, f, nil
	}
	mgr, err := multirepo.New(ctx, st, clk, nil, factory)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	if got := mgr.Repos(); len(got) != 2 || got[0] != "core" || got[1] != "web" {
		t.Fatalf("managed repos = %v, want [core web]", got)
	}
	coreEffects, err := mgr.ForRepo(ctx, "core")
	if err != nil {
		t.Fatalf("resolve core v2 effects: %v", err)
	}
	if err := coreEffects.DeleteBranch(ctx, "epic/repo-fence-proof"); err != nil {
		t.Fatal(err)
	}
	if got := fakes["core"].DeletedBranches(); len(got) != 1 || got[0] != "epic/repo-fence-proof" {
		t.Fatalf("core effect route=%v", got)
	}
	if got := fakes["web"].DeletedBranches(); len(got) != 0 {
		t.Fatalf("core effect crossed into web client: %v", got)
	}
	if _, err := mgr.ForRepo(ctx, "missing"); err == nil {
		t.Fatal("unknown v2 effect repo must fail closed")
	}

	// ── a job from EACH repo ──
	// core's job is MORE urgent (priority 1; lower = more urgent) => cross-repo
	// prioritization must offer it first, even though web's job is a different repo.
	seedReadyBuild(t, st, "jCore", "core", 1, now)
	seedReadyBuild(t, st, "jWeb", "web", 9, now)

	// ── the GLOBAL scheduler routes ANY repo's ready work to ANY capable worker ──
	// one shared, repo-agnostic worker (advertises a capability, never a repo).
	attested := []string{"role:eng_worker"}
	order, err := mgr.GlobalReadyOrder(ctx, attested, now)
	if err != nil {
		t.Fatalf("global ready order: %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("global queue should hold BOTH repos' ready jobs, got %d", len(order))
	}
	// cross-repo prioritization: core (priority 1, more urgent) outranks web (priority 9).
	if order[0].JobID != "jCore" || order[1].JobID != "jWeb" {
		t.Fatalf("cross-repo priority order = [%s %s], want [jCore jWeb]", order[0].JobID, order[1].JobID)
	}

	// the SAME shared worker pool claims both repos' jobs (repo-agnostic): one worker
	// identity claims the top of the global queue, then the next.
	for _, c := range order {
		ls, cerr := st.ClaimReadyJob(ctx, store.ClaimParams{
			JobID: c.JobID, LeaseID: "lease-" + c.JobID, Identity: "shared-box",
			ModelFamily: "claude", Role: job.RoleEngWorker, Attested: attested,
			TTL: 5 * time.Minute, Now: now,
		})
		if cerr != nil {
			t.Fatalf("shared worker claim %s: %v", c.JobID, cerr)
		}
		if ls == nil {
			t.Fatalf("claim %s returned nil lease", c.JobID)
		}
	}
	// both jobs are now leased to the one shared box (proves repo-agnostic routing).
	for _, id := range []string{"jCore", "jWeb"} {
		j, _ := st.GetJob(ctx, id)
		if j.BoundIdentity != "shared-box" {
			t.Fatalf("%s bound to %q, want shared-box (one shared pool)", id, j.BoundIdentity)
		}
	}

	// ── project-OUT runs PER repo (each sender drains only its repo, on its writer) ──
	if _, err := st.EnqueuePROpen(ctx, "jCore", "sha-core", ""); err != nil {
		t.Fatalf("enqueue core PR: %v", err)
	}
	if _, err := st.EnqueuePROpen(ctx, "jWeb", "sha-web", ""); err != nil {
		t.Fatalf("enqueue web PR: %v", err)
	}
	sent, err := mgr.DrainAll(ctx)
	if err != nil {
		t.Fatalf("drain all: %v", err)
	}
	if sent["core"] != 1 || sent["web"] != 1 {
		t.Fatalf("per-repo drain counts = %v, want core:1 web:1", sent)
	}

	// each repo's PR landed on its OWN fakeGitHub (NOT the other's): the per-repo
	// project-OUT is genuinely scoped, not a shared writer.
	coreOpens := countCalls(fakes["core"].Calls(), "OpenPR")
	webOpens := countCalls(fakes["web"].Calls(), "OpenPR")
	if coreOpens != 1 || webOpens != 1 {
		t.Fatalf("OpenPR per repo = core:%d web:%d, want 1/1 (writes did not cross repos)", coreOpens, webOpens)
	}
	// the web PR was opened against web's integration branch "trunk" (per-repo base).
	webPR := openedPR(t, fakes["web"])
	if webPR.BaseRefOid != "trunk" {
		t.Fatalf("web PR base = %q, want trunk (repo default branch)", webPR.BaseRefOid)
	}
	corePR := openedPR(t, fakes["core"])
	if corePR.BaseRefOid != "main" {
		t.Fatalf("core PR base = %q, want main", corePR.BaseRefOid)
	}

	// the PR number Flowbee stamped is repo-scoped onto each job.
	jCore, _ := st.GetJob(ctx, "jCore")
	jWeb, _ := st.GetJob(ctx, "jWeb")
	if jCore.PRNumber == 0 || jWeb.PRNumber == 0 {
		t.Fatalf("PR numbers not stamped: core=%d web=%d", jCore.PRNumber, jWeb.PRNumber)
	}

	// ── reconcile-IN runs PER repo, scoped so a PR-number COLLISION never cross-binds ──
	// Force the COLLISION case: rebind both jobs to the SAME PR number across repos,
	// then script that number on EACH fake with different facts. The per-repo sweep
	// must bind each repo's #777 to its own repo's job.
	rebindPR(t, st, "jCore", 777)
	rebindPR(t, st, "jWeb", 777)
	fakes["core"].SetPR(gh.PullRequest{
		Number: 777, HeadRefOid: "core-head", BaseRefOid: "main", CIRollup: gh.CISuccess,
		UpdatedAt: now,
	})
	fakes["web"].SetPR(gh.PullRequest{
		Number: 777, HeadRefOid: "web-head", BaseRefOid: "trunk", CIRollup: gh.CISuccess,
		UpdatedAt: now,
	})
	counts, err := mgr.SweepAll(ctx)
	if err != nil {
		t.Fatalf("sweep all: %v", err)
	}
	if counts["core"] != 1 || counts["web"] != 1 {
		t.Fatalf("per-repo sweep applied = %v, want core:1 web:1", counts)
	}

	// the keystone: each repo's #777 reconciled its OWN job's Domain-B facts — core's
	// job got core-head (NOT web-head), and vice versa. A cross-bind would have
	// written the wrong head SHA onto the wrong repo's job.
	coreFacts, ok, _ := store.DBFactSource{DB: st.DB}.Facts(ctx, "jCore")
	if !ok || coreFacts.HeadSHA != "core-head" {
		t.Fatalf("core job head = %q ok=%v, want core-head (no cross-repo bind)", coreFacts.HeadSHA, ok)
	}
	webFacts, ok, _ := store.DBFactSource{DB: st.DB}.Facts(ctx, "jWeb")
	if !ok || webFacts.HeadSHA != "web-head" {
		t.Fatalf("web job head = %q ok=%v, want web-head (no cross-repo bind)", webFacts.HeadSHA, ok)
	}

	// each repo's BoardSweep was its OWN call (per-repo reconcile loop).
	if countCalls(fakes["core"].Calls(), "BoardSweep") != 1 {
		t.Fatalf("core BoardSweep count != 1: %v", fakes["core"].Calls())
	}
	if countCalls(fakes["web"].Calls(), "BoardSweep") != 1 {
		t.Fatalf("web BoardSweep count != 1: %v", fakes["web"].Calls())
	}
}

func TestRepoLoopFailureDoesNotStopHealthyRepository(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(15_000, 0)
	for _, repoID := range []string{"a-failing", "b-healthy"} {
		if err := st.RegisterRepo(ctx, store.Repo{ID: repoID, Owner: "acme", Repo: repoID,
			DefaultBranch: "main", Active: true}); err != nil {
			t.Fatal(err)
		}
	}
	failingWriter := gh.NewFake()
	failingClient := &failingSweepClient{Fake: failingWriter, err: errors.New("github read unavailable")}
	healthy := gh.NewFake()
	mgr, err := multirepo.New(ctx, st, clock.NewFake(now), nil,
		func(repo store.Repo) (gh.Client, gh.Writer, error) {
			if repo.ID == "a-failing" {
				return failingClient, failingWriter, nil
			}
			return healthy, healthy, nil
		})
	if err != nil {
		t.Fatal(err)
	}

	counts, err := mgr.SweepAll(ctx)
	failures := multirepo.RepoFailures(err)
	if len(failures) != 1 || failures[0].RepoID != "a-failing" || failures[0].Err == nil {
		t.Fatalf("sweep failures=%+v err=%v", failures, err)
	}
	if failingClient.attempts != 1 {
		t.Fatalf("failing repo sweep attempts=%d, want one bounded attempt", failingClient.attempts)
	}
	if _, ok := counts["b-healthy"]; !ok || countCalls(healthy.Calls(), "BoardSweep") != 1 {
		t.Fatalf("healthy repo was suppressed by sibling sweep failure: counts=%v calls=%v", counts, healthy.Calls())
	}

	for _, item := range []struct{ id, repo string }{{"job-a", "a-failing"}, {"job-b", "b-healthy"}} {
		seedReadyBuild(t, st, item.id, item.repo, 1, now)
		if _, err := st.EnqueuePROpen(ctx, item.id, "sha-"+item.id, "main"); err != nil {
			t.Fatal(err)
		}
	}
	failingWriter.FailNextWriteWith(errors.New("github write unavailable"))
	drained, err := mgr.DrainAll(ctx)
	failures = multirepo.RepoFailures(err)
	if len(failures) != 1 || failures[0].RepoID != "a-failing" {
		t.Fatalf("drain failures=%+v err=%v", failures, err)
	}
	if drained["b-healthy"] != 1 || countCalls(healthy.Calls(), "OpenPR") != 1 {
		t.Fatalf("healthy repo did not drain after sibling failure: counts=%v calls=%v", drained, healthy.Calls())
	}
	if countCalls(failingWriter.Calls(), "OpenPR") != 1 {
		t.Fatalf("failing repo must be attempted exactly once: calls=%v", failingWriter.Calls())
	}
}

func TestRepositoryProbeUsesStableRepoIDAndReadOnlyGitHubSurface(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	if err := st.RegisterRepo(ctx, store.Repo{ID: "repo-stable-a", Owner: "acme", Repo: "same-name", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "repo-stable-b", Owner: "other", Repo: "same-name", Active: true}); err != nil {
		t.Fatal(err)
	}
	fakes := map[string]*gh.Fake{"repo-stable-a": gh.NewFake(), "repo-stable-b": gh.NewFake()}
	fakes["repo-stable-a"].SetPR(gh.PullRequest{Number: 9, HeadRefOid: "head-a", BaseRefOid: "base-a", CIRollup: gh.CISuccess})
	fakes["repo-stable-a"].SetPR(gh.PullRequest{Number: 2, HeadRefOid: "head-b", BaseRefOid: "base-b", CIRollup: gh.CIPending})
	mgr, err := multirepo.New(ctx, st, clock.NewFake(time.Unix(20_000, 0)), nil,
		func(repo store.Repo) (gh.Client, gh.Writer, error) { return fakes[repo.ID], fakes[repo.ID], nil })
	if err != nil {
		t.Fatal(err)
	}

	first, err := mgr.ReadRepositoryProbe(ctx, "repo-stable-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := mgr.ReadRepositoryProbe(ctx, "repo-stable-a")
	if err != nil {
		t.Fatal(err)
	}
	if first.RepoID != "repo-stable-a" || first.Fingerprint == "" || first.Fingerprint != second.Fingerprint {
		t.Fatalf("unstable mechanical facts: first=%+v second=%+v", first, second)
	}
	if first.PullRequests != 2 || first.GreenPullRequests != 1 || first.PendingPullRequests != 1 {
		t.Fatalf("unexpected facts: %+v", first)
	}
	if got := fakes["repo-stable-b"].Calls(); len(got) != 0 {
		t.Fatalf("stable-id probe crossed repository boundary: %v", got)
	}
	if _, err := mgr.ReadRepositoryProbe(ctx, "same-name"); err == nil {
		t.Fatal("owner/repo name must not be accepted as durable routing authority")
	}
	if got := fakes["repo-stable-a"].Calls(); len(got) != 2 || got[0] != "BoardSweep" || got[1] != "BoardSweep" {
		t.Fatalf("probe called a GitHub mutation surface: %v", got)
	}
}

func TestUnstickAllQueuesSelfMergeHandoffOnly(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(20_000, 0)
	clk := clock.NewFake(now)
	if err := st.RegisterRepo(ctx, store.Repo{ID: "core", Owner: "acme", Repo: "core", DefaultBranch: "main", Active: true}); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	fake := gh.NewFake()
	factory := func(r store.Repo) (gh.Client, gh.Writer, error) { return fake, fake, nil }
	mgr, err := multirepo.New(ctx, st, clk, nil, factory, multirepo.WithAutoMergeHandoff(true))
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	seedHandoff := func(id, disp string) {
		t.Helper()
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
			Repo: "core", BaseSHA: "base-" + id, Now: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		v := job.MintVerdict(job.VerdictApproved, job.Disposition(disp), "head-"+id, "base-"+id)
		vj := mustJSON(t, v)
		if _, err := st.DB.ExecContext(ctx,
			`UPDATE jobs SET state='merge_handoff', pr_number=?, head_sha=?, base_sha=?, verdict=? WHERE id=?`,
			100, "head-"+id, "base-"+id, vj, id); err != nil {
			t.Fatalf("handoff %s: %v", id, err)
		}
		if err := st.UpsertDomainBFacts(ctx, id, job.DomainBFacts{
			PRExists: true, PRNumber: 100, HeadSHA: "head-" + id, BaseSHA: "base-" + id, CIGreen: true,
		}); err != nil {
			t.Fatalf("facts %s: %v", id, err)
		}
	}
	seedHandoff("self", string(job.DispositionSelfMerge))
	seedHandoff("human", string(job.DispositionHandoff))

	counts, err := mgr.UnstickAll(ctx)
	if err != nil {
		t.Fatalf("unstick: %v", err)
	}
	if counts["core"] != 1 {
		t.Fatalf("self-merge handoff enqueue count=%v, want core:1", counts)
	}
	if got := outboxMergeCount(t, st, "self"); got != 1 {
		t.Fatalf("self merge handoff outbox rows=%d, want 1", got)
	}
	if got := outboxMergeCount(t, st, "human"); got != 0 {
		t.Fatalf("human handoff outbox rows=%d, want 0", got)
	}
}

// TestParkedRepoNotManaged: a parked (active=0) repo is omitted from the Manager —
// its loops do not run.
func TestParkedRepoNotManaged(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Unix(1, 0))

	_ = st.RegisterRepo(ctx, store.Repo{ID: "live", Owner: "a", Repo: "live", Active: true})
	_ = st.RegisterRepo(ctx, store.Repo{ID: "parked", Owner: "a", Repo: "parked", Active: false})

	fake := gh.NewFake()
	mgr, err := multirepo.New(ctx, st, clk, nil, func(store.Repo) (gh.Client, gh.Writer, error) {
		return fake, fake, nil
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	if got := mgr.Repos(); len(got) != 1 || got[0] != "live" {
		t.Fatalf("managed = %v, want [live] (parked repo excluded)", got)
	}
}

func countCalls(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return string(b)
}

func outboxMergeCount(t *testing.T, st *store.Store, jobID string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM outbox WHERE job_id=? AND action=?`, jobID, store.ActionEnqueueMerge).Scan(&n); err != nil {
		t.Fatalf("count outbox %s: %v", jobID, err)
	}
	return n
}

// rebindPR overwrites a job's bound PR number directly (the test forces a
// cross-repo PR-number collision the reconcile sweep must scope).
func rebindPR(t *testing.T, st *store.Store, jobID string, pr int) {
	t.Helper()
	if _, err := st.DB.ExecContext(context.Background(),
		`UPDATE jobs SET pr_number = ? WHERE id = ?`, pr, jobID); err != nil {
		t.Fatalf("rebind pr %s: %v", jobID, err)
	}
}

// openedPR returns the single PR opened on a fake (asserts exactly one stamped).
func openedPR(t *testing.T, f *gh.Fake) gh.PullRequest {
	t.Helper()
	for n := 1001; n < 1100; n++ {
		if pr, ok := f.PRState(n); ok {
			return pr
		}
	}
	t.Fatalf("no opened PR found on fake")
	return gh.PullRequest{}
}
