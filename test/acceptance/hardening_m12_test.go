// M12 acceptance: hardening — worker-transport mutual auth, restart-recovery, and
// the finished dashboard, proven end-to-end against a real SQLite store and an
// in-memory fakeGitHub (no real creds, no network).
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - a worker over a NON-LOOPBACK-style transport (a signed bearer token, the
//     in-env stand-in for mTLS/Tailscale per the M12 "documented, not required
//     in-env" carve-out) completes a REAL build -> review_pending, while an
//     unenrolled / tokenless non-loopback caller is rejected before it can lease;
//   - kill+restart `flowbee serve` (reopen the SAME SQLite file + a fresh server +
//     re-run reconcile-IN over the fake) reconstructs Domain-A from SQLite and
//     re-reconciles Domain-B with ZERO job loss (the in-flight job finishes to
//     done across the restart);
//   - the dashboard renders board / roster / budget / audit / cost live.
//
// mTLS / Tailscale / WAL-replication / launchd KeepAlive that need real infra are
// documented in internal/auth (MTLSConfig) and DESIGN §12.4, not exercised here.
package acceptance

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/project"
	"github.com/samhotchkiss/flowbee/internal/reconcile"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

const m12Secret = "m12-worker-auth-secret"

// newM12Server builds the private API with bearer mutual auth enabled and the
// loopback bypass OFF — so EVERY call (even over the loopback test listener) is
// forced through the token check, exactly as a non-loopback Tailscale/LAN
// listener requires. This makes the auth boundary load-bearing in the test
// without depending on a specific machine's network interfaces.
func newM12Server(t *testing.T, st *store.Store, clk clock.Clock, mirror string, enrolled []string) *api.Server {
	t.Helper()
	authn := auth.NewBearer([]byte(m12Secret), enrolled, false /* no loopback bypass */)
	return api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 500 * time.Millisecond,
		LeaseTTLS: 300, HeartbeatIntervalS: 30, HeartbeatInterval: 30 * time.Second,
		MirrorPath: mirror, Allowlist: worker.OpenAllowlist(),
		Authenticator: authn,
	}, "m12")
}

// TestM12_TransportAuthWorkerCompletesBuild proves the §7.6 trust boundary: a
// worker presenting a signed bearer token bound to an enrolled identity completes
// a REAL build (worktree at base_sha, push the epoch ref, submit -> review_pending),
// while (a) a caller with NO token and (b) a caller with a token for an UNENROLLED
// identity are both rejected 401 before they can lease any job context.
func TestM12_TransportAuthWorkerCompletesBuild(t *testing.T) {
	st := testutil.NewStore(t)
	mirrorPath, baseSHA := newBareFixture(t)
	srv := newM12Server(t, st, clock.Real{}, mirrorPath, []string{"studio.opus"})

	// a REAL listener (not httptest's 127.0.0.1-only convenience) so the request
	// path is the production one; auth is enforced regardless of the bind address
	// because the loopback bypass is off.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	hs := &http.Server{Handler: srv.PrivateHandler()}
	go hs.Serve(ln)
	defer hs.Close()
	baseURL := "http://" + ln.Addr().String()
	ctx := context.Background()

	jobID := "job-m12-auth"
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: baseSHA,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// (a) NO token -> 401 (cannot register, cannot lease).
	noTok := client.New(baseURL)
	if _, err := noTok.Register(ctx, client.Registration{
		Identity: "studio.opus", Host: "studio",
		Capabilities: []string{"role:eng_worker", "model_family:opus"},
	}); err == nil {
		t.Fatal("a tokenless caller must be rejected (401), but register succeeded")
	}

	// (b) a validly-signed token for an UNENROLLED identity -> 401. The MAC is
	// correct (same secret) but the identity is not on the allowlist.
	unenrolledTok := auth.NewBearer([]byte(m12Secret), nil, false).Mint("rogue.codex")
	rogue := client.NewWithToken(baseURL, unenrolledTok)
	if _, _, err := rogue.Lease(ctx, "rogue.codex", "codex", string(job.RoleEngWorker)); err == nil {
		t.Fatal("an unenrolled identity must be rejected before leasing")
	}
	if j, _ := st.GetJob(ctx, jobID); j.State != job.StateReady {
		t.Fatalf("job state=%s want still ready (no authorized worker yet)", j.State)
	}

	// (c) the ENROLLED worker presents its signed token and completes a real build
	// end-to-end over the authenticated transport.
	token := auth.NewBearer([]byte(m12Secret), []string{"studio.opus"}, false).Mint("studio.opus")
	out, err := worker.RunOnceHarness(ctx, worker.HarnessConfig{
		BaseURL: baseURL, Identity: "studio.opus", ModelFamily: "opus",
		AgentCmd:    "sh " + fakeAgentScript(t, "feature built over authenticated transport"),
		BearerToken: token,
	})
	if err != nil {
		t.Fatalf("authenticated harness: %v", err)
	}
	if !out.Got || out.JobID != jobID || out.JobState != string(job.StateReviewPending) {
		t.Fatalf("authenticated build got=%v job=%s state=%s want review_pending", out.Got, out.JobID, out.JobState)
	}
	// the build is real: the epoch ref was pushed and recorded as the head_ref.
	if _, ok := gitops.Open(mirrorPath).RefSHA(out.PushedRef); !ok {
		t.Fatalf("epoch ref %s not pushed over the authenticated transport", out.PushedRef)
	}
	if hr, _ := st.JobHeadRef(ctx, jobID); hr != out.PushedRef {
		t.Fatalf("recorded head_ref=%q want %s", hr, out.PushedRef)
	}
}

// TestM12_RestartRecoveryZeroJobLoss proves §12.4: the control plane holds NO
// authoritative in-memory state — Domain A lives in SQLite and Domain B is
// re-reconciled. We drive a build to mid-flight (review_pending, PR stamped),
// then KILL the process (close the store + servers), RESTART it (reopen the SAME
// SQLite file + a fresh api.Server + a fresh reconciler over the fake), and prove
// the job is reconstructed exactly and finishes to done across the restart with
// zero loss.
func TestM12_RestartRecoveryZeroJobLoss(t *testing.T) {
	dbPath := t.TempDir() + "/flowbee_m12.db"
	mirrorPath, baseSHA := newBareFixture(t)
	fake := gh.NewFake()
	ctx := context.Background()

	jobID := "job-m12-restart"
	const headSHA = "head-sha-m12"

	// ── pre-restart instance ──
	var prNum int
	{
		st, err := store.Open(ctx, dbPath)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := store.MigrateUp(ctx, st.DB); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		clk := clock.NewFake(time.Unix(1_700_000_000, 0))
		srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
			LeaseTTL: 5 * time.Minute, LongPollWait: 300 * time.Millisecond,
			LeaseTTLS: 300, HeartbeatIntervalS: 30, MirrorPath: mirrorPath,
		}, "m12")
		ts := httptest.NewServer(srv.PrivateHandler())
		sender := projectSender(st, fake, clk, srv.Broker())

		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
			Role: job.RoleEngWorker, BaseSHA: baseSHA,
			RequiredCapabilities: []string{"role:eng_worker"}, Now: clk.Now(),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}

		// the eng_worker builds -> review_pending (a real worktree+push).
		out, err := worker.RunOnceHarness(ctx, worker.HarnessConfig{
			BaseURL: ts.URL, Identity: "mac.codex", ModelFamily: "codex",
			AgentCmd: "sh " + fakeAgentScript(t, "pre-restart build"),
		})
		if err != nil || out.JobState != string(job.StateReviewPending) {
			t.Fatalf("pre-restart build err=%v state=%s", err, out.JobState)
		}

		// Flowbee opens the PR and stamps # (project-OUT). This is durable Domain-A
		// lineage that must survive the restart.
		if enq, err := st.EnqueuePROpen(ctx, jobID, headSHA, "main"); err != nil || !enq {
			t.Fatalf("enqueue PR-open enq=%v err=%v", enq, err)
		}
		drainSender(t, ctx, sender)
		prNum, _ = st.JobPR(ctx, jobID)
		if prNum == 0 {
			t.Fatalf("PR must be stamped before the kill")
		}

		// KILL: close servers + the store (simulate a crash / restart).
		ts.Close()
		if err := st.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}

	// ── post-restart instance: reopen the SAME file; reconstruct from SQLite ──
	st2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	// migrations re-run idempotently on boot (no-op on an existing DB).
	if err := store.MigrateUp(ctx, st2.DB); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	clk2 := clock.NewFake(time.Unix(1_700_001_000, 0))
	srv2 := api.New(st2, clk2, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 300 * time.Millisecond,
		LeaseTTLS: 300, HeartbeatIntervalS: 30, MirrorPath: mirrorPath,
	}, "m12")
	ts2 := httptest.NewServer(srv2.PrivateHandler())
	defer ts2.Close()
	rec2 := reconcile.New(st2, fake, clk2, srv2.Broker())

	// ZERO JOB LOSS: Domain A reconstructed exactly — same job, same state, same
	// stamped PR, same pushed head_ref.
	j, ok := mustGetJob(t, ctx, st2, jobID)
	if !ok {
		t.Fatal("the in-flight job was LOST across the restart")
	}
	if j.State != job.StateReviewPending {
		t.Fatalf("post-restart state=%s want review_pending (Domain-A preserved)", j.State)
	}
	if pr, _ := st2.JobPR(ctx, jobID); pr != prNum {
		t.Fatalf("post-restart PR=%d want %d (lineage preserved)", pr, prNum)
	}
	if hr, _ := st2.JobHeadRef(ctx, jobID); hr == "" {
		t.Fatal("post-restart head_ref lost (build product not preserved)")
	}

	// re-reconcile Domain B (fake): a NEW reviewer leases the gate on the SAME job
	// post-restart, approves; the human merges; reconcile-IN flips it to done. The
	// job completes across the restart boundary with no loss.
	// reconcile-IN supplies green facts BEFORE the reviewer leases post-restart (the
	// review gate is offered only once CI is green).
	if err := st2.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
		PRExists: true, PRNumber: prNum, HeadSHA: headSHA, BaseSHA: baseSHA, CIGreen: true,
	}); err != nil {
		t.Fatalf("reconcile facts: %v", err)
	}
	reviewer := client.New(ts2.URL)
	if _, err := reviewer.Register(ctx, client.Registration{
		Identity: "rev.opus", Host: "studio",
		Capabilities: []string{"role:code_reviewer", "model_family:opus"},
	}); err != nil {
		t.Fatalf("post-restart reviewer register: %v", err)
	}
	rg, ok2, err := reviewer.Lease(ctx, "rev.opus", "opus", string(job.RoleCodeReviewer))
	if err != nil || !ok2 || rg.JobID != jobID {
		t.Fatalf("post-restart reviewer lease ok=%v err=%v job=%s", ok2, err, rg.JobID)
	}
	rv, code, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "rev-restart", "approved", "handoff", "", "")
	if err != nil || code != http.StatusOK || !rv.Minted {
		t.Fatalf("post-restart review code=%d minted=%v err=%v", code, rv.Minted, err)
	}

	// the human merges on GitHub; the post-restart reconciler observes the terminal
	// fact and flips the job to done (Domain B re-reconciled).
	fake.SetPR(gh.PullRequest{
		Number: prNum, HeadRefOid: headSHA, BaseRefOid: baseSHA,
		Merged: true, MergeCommit: "merge-m12", CIRollup: gh.CISuccess,
		UpdatedAt: time.Unix(1_700_001_500, 0),
	})
	if _, err := rec2.Sweep(ctx); err != nil {
		t.Fatalf("post-restart sweep: %v", err)
	}
	jd, _ := st2.GetJob(ctx, jobID)
	if jd.State != job.StateDone {
		t.Fatalf("post-restart job state=%s want done (re-reconciled to terminal)", jd.State)
	}
}

// TestM12_AuditRendersLive proves the finished legacy audit UI (§12.6): /audit renders
// board + roster + budget + audit + cost in one page, and each JSON view serves
// live data. It exercises a populated store (a worker on a lease, a budget gauge,
// an audit row, a cost row) so the panes are non-empty.
func TestM12_AuditRendersLive(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	mirrorPath, baseSHA := newBareFixture(t)
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 300 * time.Millisecond,
		LeaseTTLS: 300, HeartbeatIntervalS: 30, HeartbeatInterval: 30 * time.Second,
		MirrorPath: mirrorPath, Allowlist: worker.OpenAllowlist(),
		StaleHBThreshold: 90 * time.Second,
	}, "m12")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-m12-dash"
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: baseSHA,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// a worker on a lease (board + roster panes) and a heartbeat with a cost delta
	// (cost pane).
	c := client.New(ts.URL)
	if _, err := c.Register(ctx, client.Registration{
		Identity: "dash.codex", Host: "mac-mini", Arch: "arm64", OS: "macos",
		Capabilities: []string{"role:eng_worker", "model_family:codex"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	g, ok, err := c.Lease(ctx, "dash.codex", "codex", string(job.RoleEngWorker))
	if err != nil || !ok {
		t.Fatalf("lease ok=%v err=%v", ok, err)
	}
	if _, hs, err := c.HeartbeatWith(ctx, g.JobID, g.LeaseEpoch, client.HeartbeatObs{
		TokensInDelta: 1200, TokensOutDelta: 800, MicroUSDDelta: 4500,
	}); err != nil || hs != 200 {
		t.Fatalf("heartbeat status=%d err=%v", hs, err)
	}

	// a budget gauge (reconcile-IN records it).
	if err := st.RecordRateLimit(ctx, gh.RateLimit{Remaining: 4321, Limit: 5000, ResetAt: clk.Now().Add(time.Hour)}, clk.Now()); err != nil {
		t.Fatalf("record rate limit: %v", err)
	}
	// a REAL audit row, produced by driving a project-OUT pulls.create through the
	// serialized sender (the audit log is written on send, keyed job/action/sha).
	fake := gh.NewFake()
	sender := projectSender(st, fake, clk, srv.Broker())
	if enq, err := st.EnqueuePROpen(ctx, jobID, "head-1", "main"); err != nil || !enq {
		t.Fatalf("enqueue PR-open enq=%v err=%v", enq, err)
	}
	drainSender(t, ctx, sender)

	// the JSON views are live.
	assertJSONContains(t, ts, "/v1/budget", "4321")
	assertJSONContains(t, ts, "/v1/roster", "dash.codex")
	assertJSONContains(t, ts, "/v1/cost", "4500")
	assertJSONContains(t, ts, "/v1/audit", "pulls.create")

	// The F12 audit pane renders budget/roster/cost/audit/needs-human tables live
	// off the real store. The board remains at /board; the Fleet dashboard now owns
	// the canonical /dashboard operator URL.
	html := httpGetBody(t, ts, "/audit")
	for _, want := range []string{
		"Audit",            // header
		"dash.codex",       // roster pane
		"4321",             // budget gauge
		"4500",             // cost pane (micro-USD)
		"pulls.create",     // audit pane
		"/assets/board.js", // SSE live-refresh hook (EventSource lives in the asset)
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("audit missing %q:\n%s", want, html)
		}
	}
	// the F12 board pane renders the live job card off the same store.
	board := httpGetBody(t, ts, "/board")
	for _, want := range []string{"Board", jobID, "fb-drawer", "/assets/board.js"} {
		if !strings.Contains(board, want) {
			t.Fatalf("board missing %q:\n%s", want, board)
		}
	}
}

// ── helpers ──

func projectSender(st *store.Store, fake *gh.Fake, clk *clock.Fake, b *api.Broker) *project.Sender {
	return project.New(st, fake, clk, b)
}

func drainSender(t *testing.T, ctx context.Context, s *project.Sender) {
	t.Helper()
	for {
		n, err := s.DrainOnce(ctx)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if n == 0 {
			return
		}
	}
}

func mustGetJob(t *testing.T, ctx context.Context, st *store.Store, id string) (job.Job, bool) {
	t.Helper()
	j, err := st.GetJob(ctx, id)
	if err != nil {
		return job.Job{}, false
	}
	return j, j.ID == id
}

func assertJSONContains(t *testing.T, ts *httptest.Server, path, want string) {
	t.Helper()
	body := httpGetBody(t, ts, path)
	if !strings.Contains(body, want) {
		t.Fatalf("GET %s missing %q:\n%s", path, want, body)
	}
}

func httpGetBody(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d", path, resp.StatusCode)
	}
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, e := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if e != nil {
			break
		}
	}
	return sb.String()
}
