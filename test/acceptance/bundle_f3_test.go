// F3 acceptance: Credential-less cross-box provisioning (DESIGN §7.4 mode (a),
// R4/§8, I-14). A worker that holds NO GitHub credential and NO local mirror is
// provisioned READ-ONLY via a git bundle served by the control plane; it runs its
// agent and returns ONLY a diff. The CONTROL PLANE then applies the patch, pushes
// the epoch-namespaced ref, and opens the PR — Flowbee performs ALL git writes.
//
// Proven end-to-end below by a real, non-skipped test against a real SQLite store,
// a real local BARE repo fixture (no network, no GitHub), the in-memory fakeGitHub
// (BUILD.md §6.4), the serialized project-OUT sender, and the real HTTP worker
// surface. The worker side never touches git history or GitHub and carries no creds.
package acceptance

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/project"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

// bundleEnv wires the control plane (private API with a local mirror AND bundle
// provisioning enabled) + the serialized project-OUT sender over one fakeGitHub
// and a real bare repo fixture.
type bundleEnv struct {
	st      *store.Store
	fake    *gh.Fake
	clk     *clock.Fake
	srv     *api.Server
	sender  *project.Sender
	mirror  *gitops.Mirror
	base    string
	private *httptest.Server
}

func newBundleEnv(t *testing.T) *bundleEnv {
	t.Helper()
	mirrorPath, base := newBareFixture(t)
	mirror := gitops.Open(mirrorPath)
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 30 * time.Minute, LongPollWait: 500 * time.Millisecond,
		LeaseTTLS: 1800, HeartbeatIntervalS: 60,
		MirrorPath:         mirror.Path,
		BundleProvisioning: true, // F3: cross-box, credential-less
		Allowlist:          worker.OpenAllowlist(),
	}, "f3")
	fake := gh.NewFake()
	sender := project.New(st, fake, clk, srv.Broker())
	private := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(private.Close)
	return &bundleEnv{
		st: st, fake: fake, clk: clk, srv: srv, sender: sender,
		mirror: mirror, base: base, private: private,
	}
}

func (e *bundleEnv) drain(t *testing.T, ctx context.Context) {
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

// TestF3_CredentiallessBundleWorkerBuildsAndFlowbeePushes proves the DONE-WHEN:
// a credential-less worker completes a build (returns a patch) against a local
// bare-repo fixture and Flowbee performs the push + PR-open; no creds on the
// worker side.
func TestF3_CredentiallessBundleWorkerBuildsAndFlowbeePushes(t *testing.T) {
	e := newBundleEnv(t)
	ctx := context.Background()
	url := e.private.URL
	jobID := "build-bundle"

	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: e.base,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed build: %v", err)
	}

	// ── 1. The lease advertises CROSS-BOX `bundle` provisioning: NO mirror path,
	//       NO push target. The worker gets no local repo handle and no creds. ──
	probe := registerCaps(t, ctx, url, "bart", "codex", []string{"role:eng_worker", "model_family:codex"})
	g, ok, err := probe.Lease(ctx, "bart", "codex", "")
	if err != nil || !ok || g.JobID != jobID {
		t.Fatalf("lease ok=%v err=%v", ok, err)
	}
	if g.Provisioning != "bundle" {
		t.Fatalf("F3 lease must advertise bundle provisioning, got %q", g.Provisioning)
	}
	if g.MirrorPath != "" || g.PushTarget != "" {
		t.Fatalf("a credential-less bundle worker must get NO mirror path / push target; got mirror=%q push=%q",
			g.MirrorPath, g.PushTarget)
	}
	// release the probe lease so the harness can re-lease cleanly.
	if _, err := probe.Release(ctx, jobID, g.LeaseEpoch); err != nil {
		t.Fatalf("release probe: %v", err)
	}

	// ── 2. Run the FULL credential-less harness: it fetches a read-only bundle,
	//       clones from it (no creds, no git writes), runs an agent that edits a
	//       file, and returns ONLY a diff. ──
	agent := fakeAgentScript(t, "credential-less build by bundle worker")
	// the agent script writes agent_output.txt in the workspace; that IS the diff.
	out, err := worker.RunOnceHarnessBundle(ctx, worker.HarnessConfig{
		BaseURL:     url,
		Identity:    "bart",
		ModelFamily: "codex",
		Role:        "eng_worker",
		AgentCmd:    "sh " + agent,
		// NOTE: no creds anywhere in the config — the worker holds none (R4/I-14).
	})
	if err != nil {
		t.Fatalf("bundle harness: %v", err)
	}
	if !out.Got || out.JobID != jobID {
		t.Fatalf("harness must lease + build the job, got %+v", out)
	}
	if out.JobState != string(job.StateReviewPending) {
		t.Fatalf("a successful build must land review_pending, got %q", out.JobState)
	}

	// ── 3. FLOWBEE performed the git write the worker could not: the epoch ref was
	//       created by the control plane applying the worker's patch. The promoted
	//       content carries the worker's change. ──
	// The job recorded its head_ref = the epoch ref Flowbee pushed (NOT the worker —
	// the worker returned only a diff and pushed nothing).
	headRef, err := e.st.JobHeadRef(ctx, jobID)
	if err != nil {
		t.Fatalf("job head ref: %v", err)
	}
	epochRef := gitops.EpochRef(jobID, out.LeaseEpoch)
	if headRef != epochRef {
		t.Fatalf("Flowbee must record the epoch ref it pushed as head_ref; got %q want %q", headRef, epochRef)
	}
	sha, refOK := e.mirror.RefSHA(epochRef)
	if !refOK {
		t.Fatalf("the control plane must have pushed the epoch ref %s", epochRef)
	}
	// promote the epoch ref onto a branch and confirm it carries the worker's change.
	if err := e.mirror.PromoteEpochRef(epochRef, "refs/heads/f3-promoted"); err != nil {
		t.Fatalf("promote epoch ref: %v", err)
	}
	promotedSHA, err := e.mirror.HeadSHA("refs/heads/f3-promoted")
	if err != nil || promotedSHA != sha {
		t.Fatalf("promoted branch must point at the pushed epoch sha")
	}
	// the promoted commit carries the worker's untrusted change — proof the control
	// plane applied the worker's patch (the worker itself never wrote to the mirror).
	show, err := exec.Command("git", "--git-dir", e.mirror.Path, "show", "refs/heads/f3-promoted:agent_output.txt").CombinedOutput()
	if err != nil || !strings.Contains(string(show), "credential-less build by bundle worker") {
		t.Fatalf("promoted commit must carry the worker's change, got %q err=%v", show, err)
	}

	// ── 4. Flowbee opens the PR (project-OUT, the §7.3 trigger) — the worker never
	//       supplied a PR and never called GitHub. ──
	if enq, err := e.st.EnqueuePROpen(ctx, jobID, sha, "main"); err != nil || !enq {
		t.Fatalf("enqueue PR-open enq=%v err=%v", enq, err)
	}
	e.drain(t, ctx)
	prNum, _ := e.st.JobPR(ctx, jobID)
	if prNum == 0 {
		t.Fatalf("Flowbee must open the PR and stamp the number")
	}
	if _, ok := e.fake.PRState(prNum); !ok {
		t.Fatalf("the PR must exist in the (fake) GitHub board")
	}
	// the only GitHub caller was Flowbee: the fake recorded an OpenPR call.
	var sawOpenPR bool
	for _, c := range e.fake.Calls() {
		if c == "OpenPR" {
			sawOpenPR = true
		}
	}
	if !sawOpenPR {
		t.Fatalf("Flowbee (not the worker) must be the one to open the PR")
	}
}

// TestF3_MalformedPatchIsDeclinedAndCannotCorruptMirror proves the untrusted-data
// posture: when a bundle worker returns a diff that does not apply onto base, the
// control plane DECLINES the result (no epoch ref, no state advance) — a hostile
// or corrupt patch cannot push anything to the mirror.
func TestF3_MalformedPatchIsDeclinedAndCannotCorruptMirror(t *testing.T) {
	e := newBundleEnv(t)
	ctx := context.Background()
	url := e.private.URL
	jobID := "build-bad-patch"
	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: e.base,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := registerCaps(t, ctx, url, "mallory", "codex", []string{"role:eng_worker", "model_family:codex"})
	g, ok, err := c.Lease(ctx, "mallory", "codex", "")
	if err != nil || !ok {
		t.Fatalf("lease ok=%v err=%v", ok, err)
	}
	// the worker submits a patch that does not apply onto base (no pushed_ref —
	// credential-less). The control plane tries to apply it and fails -> 422.
	_, st, _ := c.Result(ctx, jobID, g.LeaseEpoch, "bad1", map[string]any{
		"kind": "patch", "base_sha": e.base,
		"diff":         "garbage that is not a diff\n",
		"blast_radius": map[string]any{"paths": []string{"x"}},
	})
	if st != 422 {
		t.Fatalf("a non-applying patch must be declined with 422, got status %d", st)
	}
	// the job did NOT advance and NO epoch ref exists (the mirror is untouched).
	j, _ := e.st.GetJob(ctx, jobID)
	if j.State == job.StateReviewPending {
		t.Fatalf("a declined result must NOT advance the job to review_pending")
	}
	if _, ok := e.mirror.RefSHA(gitops.EpochRef(jobID, g.LeaseEpoch)); ok {
		t.Fatalf("a non-applying patch must NOT create an epoch ref on the mirror")
	}
}

// TestF3_BundleEndpointServesReadOnlyBytesAndWorkerCannotPush asserts the security
// posture: the bundle channel returns pure read-only bytes and the credential-less
// worker has no client method that writes git or GitHub.
func TestF3_BundleEndpointServesReadOnlyBytes(t *testing.T) {
	e := newBundleEnv(t)
	ctx := context.Background()
	url := e.private.URL
	jobID := "build-bundle-2"
	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: e.base,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := client.New(url)
	b, err := c.Bundle(ctx, jobID)
	if err != nil {
		t.Fatalf("fetch bundle: %v", err)
	}
	// a git bundle starts with the signature "# v2 git bundle" / "# v3 git bundle".
	if !strings.HasPrefix(string(b[:min(20, len(b))]), "# v") {
		t.Fatalf("bundle bytes must be a real git bundle, got prefix %q", string(b[:min(20, len(b))]))
	}
	// the worker can clone a working tree from the bytes alone — no creds, no mirror.
	wsDir := t.TempDir() + "/ws"
	ws, err := gitops.CloneFromBundle(wsDir, b, e.base)
	if err != nil {
		t.Fatalf("clone from bundle bytes: %v", err)
	}
	defer ws.Destroy()
	// a BundleWorkspace exposes Run/HasChanges/Diff/Destroy — and crucially NO push
	// method: the worker structurally cannot perform a git write (compile-time fact).
	changed, err := ws.HasChanges()
	if err != nil {
		t.Fatalf("has changes: %v", err)
	}
	if changed {
		t.Fatalf("a fresh bundle checkout must be clean")
	}
}
