// F2 acceptance: autonomous-merge config (Branch B) + content-policy config.
//
// THE ONE DECISION (§14) is RESOLVED to Branch B as the production posture, made
// CONFIGURABLE here: config.Config.AllowSelfMerge (env FLOWBEE_ALLOW_SELF_MERGE)
// flows into api.Config.Policy exactly as cmd/flowbee/serve.go wires it. The
// content-integrity ceilings + extra denylist are configurable the same way.
//
// DONE-WHEN (each proven below by a real, non-skipped test against the in-memory
// fakeGitHub — no real creds, no network):
//   - with FLOWBEE_ALLOW_SELF_MERGE=true, an approved + denylist-clear + CI-green
//     job is self_merge-eligible: the gate mints the SHA-bound verdict, the job
//     reaches `merging` (NOT handoff), and Flowbee merges it AUTONOMOUSLY — the PR
//     is enqueued to GitHub's native merge queue via the fakeGitHub Writer, with no
//     human in the loop;
//   - the DEFAULT (env unset / false) stays Branch A: the SAME approved job is
//     forced to merge_handoff (a human must merge), proving the flip is pure policy;
//   - a configured EXTRA-denylist path forces handoff even under Branch B, proving
//     the content-policy config is honored by the live gate.
package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/content"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/project"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// f2Env wires the control plane (private API) + the serialized project-OUT sender
// over ONE scriptable fakeGitHub, with the api.Config built from a config.Config
// EXACTLY the way cmd/flowbee/serve.go does (the wiring under test).
type f2Env struct {
	st      *store.Store
	fake    *gh.Fake
	clk     *clock.Fake
	sender  *project.Sender
	private *httptest.Server
}

// newF2Env builds the runtime from a config.Config, mirroring serve.go's mapping of
// config -> api.Config.Policy + api.Config.ContentPolicy.
func newF2Env(t *testing.T, cfg config.Config) *f2Env {
	t.Helper()
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 500 * time.Millisecond,
		LeaseTTLS: 300, HeartbeatIntervalS: 30,
		// the exact serve.go wiring (F2):
		Policy:        job.Policy{AllowSelfMerge: cfg.AllowSelfMerge},
		ContentPolicy: cfg.ContentPolicy(),
	}, "f2")
	fake := gh.NewFake()
	sender := project.New(st, fake, clk, srv.Broker())
	sender.WithHistory(fakeMergeHistory{}, "main") // self-merge requires a mirror to pin+re-verify
	private := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(private.Close)
	return &f2Env{st: st, fake: fake, clk: clk, sender: sender, private: private}
}

func (e *f2Env) drain(t *testing.T, ctx context.Context) int {
	t.Helper()
	total := 0
	for {
		n, err := e.sender.DrainOnce(ctx)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		total += n
		if n == 0 {
			return total
		}
	}
}

// driveApprovedCleanJob takes a build job from `build` all the way to a minted
// approval over a CLEAN diff with green reconciled facts + a self_merge request.
// Returns the reviewer's ReviewResponse (which carries the post-gate JobState) and
// the stamped PR number. It opens the PR via project-OUT so an actual merge has a
// real PR to enqueue (the autonomous-merge side-effect we assert on).
func driveApprovedCleanJob(t *testing.T, ctx context.Context, e *f2Env, jobID, diff string, decl content.BlastRadius) (client.ReviewResponse, int) {
	t.Helper()
	url := e.private.URL

	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base-sha-0",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// the eng_worker builds, pushes its epoch ref, posts the UNTRUSTED diff + blast.
	builder := registerWorker(t, ctx, url, "builder-bob", "codex")
	bg, ok, err := builder.Lease(ctx, "builder-bob", "codex", "")
	if err != nil || !ok || bg.JobID != jobID {
		t.Fatalf("builder lease ok=%v err=%v job=%s", ok, err, bg.JobID)
	}
	brJSON, _ := json.Marshal(decl)
	if _, _, err := builder.Result(ctx, jobID, bg.LeaseEpoch, "build-1", map[string]any{
		"kind": "patch", "base_sha": "base-sha-0",
		"pushed_ref": "refs/flowbee/" + jobID + "/epoch-1",
		"diff":       diff, "blast_radius": json.RawMessage(brJSON),
	}); err != nil {
		t.Fatalf("builder result: %v", err)
	}

	// Flowbee opens the PR and stamps the number (§7.3) — the worker never does.
	headSHA := "head-sha-1"
	if enq, err := e.st.EnqueuePROpen(ctx, jobID, headSHA, "main"); err != nil || !enq {
		t.Fatalf("enqueue PR-open enq=%v err=%v", enq, err)
	}
	if e.drain(t, ctx) != 1 {
		t.Fatalf("exactly one pulls.create expected")
	}
	prNum, _ := e.st.JobPR(ctx, jobID)
	if prNum == 0 {
		t.Fatalf("Flowbee must open the PR and stamp the number")
	}

	// reconcile-IN supplies GREEN facts BEFORE the reviewer leases: the review gate
	// is only offered once CI is green (ReviewPendingCandidates), matching production
	// where reconcile writes facts before a code_reviewer would pick the job up.
	if err := e.st.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
		PRExists: true, PRNumber: prNum, HeadSHA: headSHA, BaseSHA: "base-sha-0", CIGreen: true,
	}); err != nil {
		t.Fatalf("reconcile facts: %v", err)
	}
	e.fake.SetPR(gh.PullRequest{
		Number: prNum, HeadRefOid: headSHA, BaseRefOid: "base-sha-0",
		CIRollup: gh.CISuccess, PassedChecks: []string{"acceptance"},
	})

	// a DISTINCT reviewer leases the gate.
	reviewer := client.New(url)
	if _, err := reviewer.Register(ctx, client.Registration{
		WorkerID: "wk-rev", Identity: "rev", Host: "t",
		Capabilities: []string{"role:code_reviewer", "model_family:opus"},
	}); err != nil {
		t.Fatalf("reviewer register: %v", err)
	}
	rg, ok, err := reviewer.Lease(ctx, "rev", "opus", string(job.RoleCodeReviewer))
	if err != nil || !ok || rg.JobID != jobID {
		t.Fatalf("reviewer lease ok=%v err=%v", ok, err)
	}

	rv, code, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "rev-1", "approved", "self_merge", "", "")
	if err != nil || code != http.StatusOK {
		t.Fatalf("review code=%d err=%v", code, err)
	}
	if !rv.Minted || rv.Verdict != "approved" {
		t.Fatalf("a green approval must mint regardless of policy (I-9), got %+v", rv)
	}
	return rv, prNum
}

// cleanDiff is a real unified diff touching exactly one ordinary (denylist-clear)
// source path, fully covered by its declaration.
func cleanDiff(path string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"--- a/" + path + "\n" +
		"+++ b/" + path + "\n" +
		"@@ -1 +1,2 @@\n first\n+second\n"
}

// TestF2_AutonomousMergeWhenEnvTrue is the keystone: FLOWBEE_ALLOW_SELF_MERGE=true
// makes an approved + denylist-clear + CI-green job self_merge-eligible and Flowbee
// merges it AUTONOMOUSLY (enqueued to GitHub's merge queue via fakeGitHub) — no
// human gate.
func TestF2_AutonomousMergeWhenEnvTrue(t *testing.T) {
	t.Setenv("FLOWBEE_ALLOW_SELF_MERGE", "true")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !cfg.AllowSelfMerge {
		t.Fatalf("FLOWBEE_ALLOW_SELF_MERGE=true must set AllowSelfMerge")
	}
	e := newF2Env(t, cfg)
	ctx := context.Background()

	jobID := "f2-auto"
	decl := content.BlastRadius{Paths: []string{"pkg/app/handler.go"}}
	rv, prNum := driveApprovedCleanJob(t, ctx, e, jobID, cleanDiff("pkg/app/handler.go"), decl)

	// Branch B: the §5.4 predicate holds (policy on + content-clear + SHA-bound
	// verdict + CI green), so the job reaches `merging` — NOT merge_handoff.
	if rv.JobState != string(job.StateMerging) {
		t.Fatalf("Branch B: an eligible approval must reach merging, got %s", rv.JobState)
	}
	j, _ := e.st.GetJob(ctx, jobID)
	if j.State != job.StateMerging {
		t.Fatalf("projection state=%s want merging", j.State)
	}
	if j.Verdict == nil || j.Verdict.Disposition != job.DispositionSelfMerge {
		t.Fatalf("the minted verdict must carry self_merge under Branch B, got %+v", j.Verdict)
	}

	// Flowbee merges AUTONOMOUSLY: it enqueues the PR to GitHub's native merge queue
	// (both arms physically merge via the queue, §5.4). The worker holds NO GitHub
	// creds — Flowbee does the write (F3). Proven by the fakeGitHub merge-queue call.
	menq, err := e.st.EnqueueMergeForJob(ctx, jobID, e.clk.Now())
	if err != nil || !menq {
		t.Fatalf("enqueue merge enq=%v err=%v", menq, err)
	}
	e.drain(t, ctx)
	if eq := e.fake.Enqueued(); len(eq) != 1 || eq[0] != prNum {
		t.Fatalf("autonomous merge: the PR must be enqueued to the merge queue once, got %v", eq)
	}
	// audited exactly once, keyed (job, action, head_sha) — no human anywhere.
	audit, _ := e.st.AuditLog(ctx, jobID)
	if countAction(audit, store.ActionEnqueueMerge) != 1 {
		t.Fatalf("the autonomous merge must be audited exactly once")
	}
}

// TestF2_DefaultStaysHandoff proves the default (env unset/false) is Branch A: the
// SAME approved + clean + green job is forced to merge_handoff (a human merges).
// The flip is pure policy — identical code path, different config.
func TestF2_DefaultStaysHandoff(t *testing.T) {
	// explicitly unset so a parallel/leaked env cannot leak Branch B in.
	t.Setenv("FLOWBEE_ALLOW_SELF_MERGE", "")
	cfg := config.Default()
	if cfg.AllowSelfMerge {
		t.Fatalf("default config must be Branch A (self-merge off)")
	}
	e := newF2Env(t, cfg)
	ctx := context.Background()

	jobID := "f2-handoff"
	decl := content.BlastRadius{Paths: []string{"pkg/app/handler.go"}}
	rv, _ := driveApprovedCleanJob(t, ctx, e, jobID, cleanDiff("pkg/app/handler.go"), decl)

	// Branch A: even a perfectly clean, green, self_merge-requested approval is
	// forced to handoff — a human must merge.
	if rv.JobState != string(job.StateMergeHandoff) {
		t.Fatalf("Branch A: an approval must reach merge_handoff, got %s", rv.JobState)
	}
	j, _ := e.st.GetJob(ctx, jobID)
	if j.Verdict == nil || j.Verdict.Disposition == job.DispositionSelfMerge {
		t.Fatalf("Branch A must never mint a self_merge disposition, got %+v", j.Verdict)
	}
}

// TestF2_ConfiguredDenylistForcesHandoffUnderBranchB proves the content-policy
// config is honored by the LIVE gate: with Branch B ON, a diff touching an
// operator-configured EXTRA-denylist prefix is forced to handoff (the human gate),
// even though policy + CI would otherwise allow self_merge.
func TestF2_ConfiguredDenylistForcesHandoffUnderBranchB(t *testing.T) {
	cfg := config.Default()
	cfg.AllowSelfMerge = true                      // Branch B
	cfg.ContentDenyExtra = []string{"migrations/"} // operator extra denylist
	e := newF2Env(t, cfg)
	ctx := context.Background()

	jobID := "f2-deny"
	// the diff touches a configured-protected path; the worker declares it honestly,
	// so the ONLY thing denying self_merge is the configured denylist (not a tamper
	// or static failure).
	decl := content.BlastRadius{Paths: []string{"migrations/001_init.sql"}}
	rv, _ := driveApprovedCleanJob(t, ctx, e, jobID, cleanDiff("migrations/001_init.sql"), decl)

	if rv.JobState != string(job.StateMergeHandoff) {
		t.Fatalf("a configured-denylist path must force handoff under Branch B, got %s", rv.JobState)
	}
	// the cached content Result records the configured hit (audit trail).
	r := loadContentResult(t, e.st, jobID)
	if r.DenylistClear {
		t.Fatalf("the content gate must record the configured denylist hit, got clear")
	}
	sawConfigured := false
	for _, h := range r.DenylistHits {
		if h == "configured:migrations/001_init.sql" {
			sawConfigured = true
		}
	}
	if !sawConfigured {
		t.Fatalf("expected a configured-denylist hit, got %v", r.DenylistHits)
	}
}
