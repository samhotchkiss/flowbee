// M5 acceptance: real worker harness + attestation + repo provisioning + BOTH
// worker modes, proven end-to-end over the real HTTP surface against a real
// SQLite store and a LOCAL bare-repo fixture (no network, no GitHub).
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - flowbee work (Mode A) with a FAKE agent CLI leases a build job, provisions
//     a git worktree at base_sha off the bare mirror, pushes
//     refs/flowbee/<job>/epoch-<n>, and submits a REAL patch -> review_pending;
//   - a Mode-B session (flowbee lease / flowbee submit, the real binary)
//     completes the same kind of build via worktree+push+submit;
//   - an UNATTESTED capability never matches (a strict allowlist drops an
//     unenrolled role claim; the worker is never offered the job);
//   - the roster shows BOTH workers and a stale-heartbeat badge.
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

// newBareFixture builds a local bare repo with one commit on main and returns
// the mirror path and the base SHA of main (DESIGN §7.4 bare-repo fixture).
func newBareFixture(t *testing.T) (mirrorPath, baseSHA string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "mirror.git")
	m, err := gitops.InitBare(bare)
	if err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := filepath.Join(root, "seed")
	runGit(t, "", "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
	runGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	runGit(t, work, "branch", "-M", "main")
	runGit(t, work, "push", "origin", "main")
	sha, err := m.HeadSHA("refs/heads/main")
	if err != nil {
		t.Fatalf("head sha: %v", err)
	}
	return bare, sha
}

// fakeAgentScript writes a shell "agent CLI" that, when run in a worktree,
// produces a deterministic change (the provider-agnostic black box of §7.1).
func fakeAgentScript(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sh")
	body := "#!/bin/sh\nset -e\nprintf '%s' \"" + content + "\" > agent_output.txt\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newM5Server(st *store.Store, clk clock.Clock, mirror string, allow worker.Allowlist) *api.Server {
	return api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 500 * time.Millisecond,
		LeaseTTLS: 300, HeartbeatIntervalS: 30, HeartbeatInterval: 30 * time.Second,
		MirrorPath: mirror, Allowlist: allow,
	}, "m5")
}

// TestM5ModeAHarnessBuildsAndPushesEpochRef proves the Mode-A harness end-to-end:
// a real worktree at base_sha, a fake agent CLI produces a change, the harness
// pushes refs/flowbee/<job>/epoch-<n> and submits a real patch -> review_pending.
func TestM5ModeAHarnessBuildsAndPushesEpochRef(t *testing.T) {
	st := testutil.NewStore(t)
	mirrorPath, baseSHA := newBareFixture(t)
	srv := newM5Server(st, clock.Real{}, mirrorPath, worker.OpenAllowlist())
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-modea"
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: baseSHA,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	agent := fakeAgentScript(t, "feature built by mode-a agent")
	out, err := worker.RunOnceHarness(ctx, worker.HarnessConfig{
		BaseURL: ts.URL, Identity: "mac-mini.codex", ModelFamily: "codex",
		AgentCmd: "sh " + agent,
	})
	if err != nil {
		t.Fatalf("harness: %v", err)
	}
	if !out.Got || out.JobID != jobID {
		t.Fatalf("harness got=%v job=%s want %s", out.Got, out.JobID, jobID)
	}
	if out.JobState != string(job.StateReviewPending) {
		t.Fatalf("job state=%s want review_pending", out.JobState)
	}
	if out.PushedRef != gitops.EpochRef(jobID, out.LeaseEpoch) {
		t.Fatalf("pushed ref=%s want %s", out.PushedRef, gitops.EpochRef(jobID, out.LeaseEpoch))
	}

	// the epoch ref exists in the mirror at the pushed SHA, carrying the change.
	m := gitops.Open(mirrorPath)
	gotSHA, ok := m.RefSHA(out.PushedRef)
	if !ok || gotSHA != out.PushedSHA {
		t.Fatalf("epoch ref sha=%q ok=%v want %s", gotSHA, ok, out.PushedSHA)
	}
	// the control plane recorded the pushed ref as the job's head_ref (the patch
	// is real — §7.3), and Flowbee can promote it onto a real branch (the worker
	// never did; it only pushed the epoch ref).
	headRef, _ := st.JobHeadRef(ctx, jobID)
	if headRef != out.PushedRef {
		t.Fatalf("recorded head_ref=%q want %s", headRef, out.PushedRef)
	}
	if err := m.PromoteEpochRef(out.PushedRef, "refs/heads/"+jobID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted, ok := m.RefSHA("refs/heads/" + jobID); !ok || promoted != out.PushedSHA {
		t.Fatalf("promoted branch sha=%q want %s", promoted, out.PushedSHA)
	}
}

// TestM5ModeBLeaseSubmitCompletesBuild proves Mode B via the REAL flowbee binary:
// `flowbee lease` prints the grant (with the mirror + base_sha + push target),
// `flowbee submit` provisions a worktree, runs the fake agent, pushes the epoch
// ref, and submits -> review_pending. No DB access from the client (R4).
func TestM5ModeBLeaseSubmitCompletesBuild(t *testing.T) {
	st := testutil.NewStore(t)
	mirrorPath, baseSHA := newBareFixture(t)
	srv := newM5Server(st, clock.Real{}, mirrorPath, worker.OpenAllowlist())
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	bin := buildFlowbee(t)

	jobID := "job-modeb"
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: baseSHA,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Mode-B step 1: register (via the in-process client; the binary's lease needs
	// the identity enrolled). Then `flowbee lease` to obtain the grant.
	c := client.New(ts.URL)
	if _, err := c.Register(ctx, client.Registration{
		Identity: "studio.opus", Host: "studio",
		Capabilities: []string{"role:eng_worker", "model_family:opus"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	leaseOut := runFlowbee(t, bin, map[string]string{"FLOWBEE_URL": ts.URL},
		"lease", "--identity", "studio.opus", "--model-family", "opus")
	var grant client.LeaseGrant
	if err := json.Unmarshal([]byte(strings.TrimSpace(leaseOut)), &grant); err != nil {
		t.Fatalf("parse lease output %q: %v", leaseOut, err)
	}
	if grant.JobID != jobID || grant.MirrorPath != mirrorPath || grant.BaseSHA != baseSHA {
		t.Fatalf("grant=%+v want job=%s mirror=%s base=%s", grant, jobID, mirrorPath, baseSHA)
	}

	// Mode-B step 2: `flowbee submit` provisions + pushes + submits a REAL patch.
	agent := fakeAgentScript(t, "feature built by mode-b agent")
	submitOut := runFlowbee(t, bin, map[string]string{"FLOWBEE_URL": ts.URL},
		"submit", "--action", "result", "--job", grant.JobID,
		"--epoch", itoa(grant.LeaseEpoch), "--mirror", grant.MirrorPath,
		"--base-sha", grant.BaseSHA, "--agent-cmd", "sh "+agent,
		"--idempotency-key", "modeb-1", "--identity", "studio.opus")
	if !strings.Contains(submitOut, "job_state=review_pending") {
		t.Fatalf("submit output missing review_pending:\n%s", submitOut)
	}

	j, _ := st.GetJob(ctx, jobID)
	if j.State != job.StateReviewPending {
		t.Fatalf("job state=%s want review_pending", j.State)
	}
	headRef, _ := st.JobHeadRef(ctx, jobID)
	if headRef != gitops.EpochRef(jobID, grant.LeaseEpoch) {
		t.Fatalf("head_ref=%q want %s", headRef, gitops.EpochRef(jobID, grant.LeaseEpoch))
	}
	if _, ok := gitops.Open(mirrorPath).RefSHA(headRef); !ok {
		t.Fatalf("epoch ref %s not pushed by mode-b", headRef)
	}
}

// TestM5UnattestedCapabilityNeverMatches proves the attestation boundary: a
// strict allowlist enrolls a worker for role:eng_worker only; a DIFFERENT worker
// CLAIMS role:eng_worker but is NOT enrolled, so its claim is not attested and it
// is never offered the job (its lease long-poll returns 204). The enrolled worker
// wins.
func TestM5UnattestedCapabilityNeverMatches(t *testing.T) {
	st := testutil.NewStore(t)
	mirrorPath, baseSHA := newBareFixture(t)
	allow := worker.Allowlist{Permit: map[string][]string{
		"enrolled.codex": {"role:eng_worker", "model_family:codex"},
	}}
	srv := newM5Server(st, clock.Real{}, mirrorPath, allow)
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-attest"
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: baseSHA,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// the UNENROLLED worker claims role:eng_worker but cannot attest it.
	intruder := client.New(ts.URL)
	if _, err := intruder.Register(ctx, client.Registration{
		Identity: "intruder.codex", Host: "rogue",
		Capabilities: []string{"role:eng_worker", "model_family:codex"},
	}); err != nil {
		t.Fatalf("register intruder: %v", err)
	}
	if g, ok, err := intruder.Lease(ctx, "intruder.codex", "codex", string(job.RoleEngWorker)); err != nil || ok {
		t.Fatalf("unattested worker must NOT win: ok=%v job=%s err=%v", ok, g.JobID, err)
	}
	if j, _ := st.GetJob(ctx, jobID); j.State != job.StateReady {
		t.Fatalf("job state=%s want still ready (no eligible attested worker)", j.State)
	}

	// the ENROLLED worker attests role:eng_worker and wins.
	enrolled := client.New(ts.URL)
	if _, err := enrolled.Register(ctx, client.Registration{
		Identity: "enrolled.codex", Host: "mac",
		Capabilities: []string{"role:eng_worker", "model_family:codex"},
	}); err != nil {
		t.Fatalf("register enrolled: %v", err)
	}
	g, ok, err := enrolled.Lease(ctx, "enrolled.codex", "codex", string(job.RoleEngWorker))
	if err != nil || !ok || g.JobID != jobID {
		t.Fatalf("enrolled attested worker must win: ok=%v err=%v job=%s", ok, err, g.JobID)
	}
}

// TestM5RosterShowsBothWorkersAndStaleBadge proves the roster (§12.6.2): two
// enrolled workers appear; the one that hasn't heartbeated past the threshold is
// badged stale-hb (the worker-partitioned signal), while a freshly-heartbeating
// worker is not.
func TestM5RosterShowsBothWorkersAndStaleBadge(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(10000, 0))
	mirrorPath, baseSHA := newBareFixture(t)
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 300 * time.Millisecond,
		LeaseTTLS: 300, HeartbeatIntervalS: 30, HeartbeatInterval: 30 * time.Second,
		MirrorPath: mirrorPath, Allowlist: worker.OpenAllowlist(),
		StaleHBThreshold: 90 * time.Second,
	}, "m5")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	// worker A is fresh (registers at t=10000 and will heartbeat on its lease).
	jobID := "job-roster"
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: baseSHA,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := client.New(ts.URL)
	if _, err := a.Register(ctx, client.Registration{
		Identity: "fresh.codex", Host: "mac-mini", Arch: "arm64", OS: "macos",
		Capabilities: []string{"role:eng_worker", "model_family:codex", "arch:arm64", "os:macos"},
	}); err != nil {
		t.Fatalf("register A: %v", err)
	}
	g, ok, err := a.Lease(ctx, "fresh.codex", "codex", string(job.RoleEngWorker))
	if err != nil || !ok {
		t.Fatalf("A lease ok=%v err=%v", ok, err)
	}
	// A heartbeats at the current clock -> its last_seen is fresh.
	if _, hs, err := a.Heartbeat(ctx, g.JobID, g.LeaseEpoch); err != nil || hs != 200 {
		t.Fatalf("A heartbeat status=%d err=%v", hs, err)
	}

	// worker B registered earlier and never heartbeated; advance the clock so B's
	// last_seen is now stale (> 90s), while A just heartbeated at the new "now".
	b := client.New(ts.URL)
	if _, err := b.Register(ctx, client.Registration{
		Identity: "stale.opus", Host: "iMac", Arch: "x86_64", OS: "macos",
		Capabilities: []string{"role:code_reviewer", "model_family:opus", "arch:x86_64", "os:macos"},
	}); err != nil {
		t.Fatalf("register B: %v", err)
	}
	clk.Advance(2 * time.Minute)
	// A heartbeats again at the advanced clock to refresh its last_seen.
	if _, hs, err := a.Heartbeat(ctx, g.JobID, g.LeaseEpoch); err != nil || hs != 200 {
		t.Fatalf("A heartbeat 2 status=%d err=%v", hs, err)
	}

	roster, err := st.Roster(ctx, clk.Now(), 90*time.Second)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	byID := map[string]store.RosterWorker{}
	for _, r := range roster {
		byID[r.Identity] = r
	}
	if len(byID) != 2 {
		t.Fatalf("roster has %d workers want 2: %+v", len(byID), roster)
	}
	fresh, ok := byID["fresh.codex"]
	if !ok || fresh.StaleHB {
		t.Fatalf("fresh worker badged stale or missing: %+v", fresh)
	}
	if fresh.ActiveJob != jobID {
		t.Fatalf("fresh worker active job=%q want %s", fresh.ActiveJob, jobID)
	}
	stale, ok := byID["stale.opus"]
	if !ok || !stale.StaleHB {
		t.Fatalf("stale worker NOT badged stale-hb: %+v", stale)
	}

	// the HTML roster page renders both + the badge.
	resp, err := ts.Client().Get(ts.URL + "/roster")
	if err != nil {
		t.Fatalf("GET /roster: %v", err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	html := buf.String()
	if !strings.Contains(html, "fresh.codex") || !strings.Contains(html, "stale.opus") {
		t.Fatalf("roster page missing a worker:\n%s", html)
	}
	if !strings.Contains(html, "stale-hb") {
		t.Fatalf("roster page missing stale-hb badge:\n%s", html)
	}
}

// ── helpers ──

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// buildFlowbee compiles the flowbee binary once for the Mode-B CLI tests.
func buildFlowbee(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "flowbee")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/samhotchkiss/flowbee/cmd/flowbee")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build flowbee: %v: %s", err, out)
	}
	return bin
}

func runFlowbee(t *testing.T, bin string, env map[string]string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("flowbee %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, out.String(), errb.String())
	}
	return out.String()
}

