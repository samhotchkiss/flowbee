// M7 acceptance: project-OUT outbox + spec flow + ADOPT mode, proven end-to-end
// against an in-memory fakeGitHub (BUILD.md §6.4 — no real creds, no network) over
// the real HTTP worker surface, the serialized project-OUT sender, and reconcile-IN.
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - a spec doc -> spec job commits spec.md + a BLAKE3 hash, gates on a
//     distinct-lens reviewer;
//   - editing the spec supersedes the prior sign-off;
//   - sign-off MATERIALIZES a real issue (project-OUT);
//   - a builder patch -> Flowbee opens the PR and stamps the number;
//   - reviewer approves -> handoff/needs_human;
//   - the human-merges fact reconciles to done;
//   - pre-existing PRs import quiescent + untouched;
//   - every GitHub action appears once in the audit log keyed (job_id, action, head_sha).
package acceptance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/project"
	"github.com/samhotchkiss/flowbee/internal/reconcile"
	"github.com/samhotchkiss/flowbee/internal/spec"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// m7Env wires the control plane (private API), the serialized project-OUT sender,
// and reconcile-IN — all against ONE scriptable fakeGitHub (it satisfies both the
// reconcile Client and the project-OUT Writer).
type m7Env struct {
	st      *store.Store
	fake    *gh.Fake
	clk     *clock.Fake
	srv     *api.Server
	sender  *project.Sender
	rec     *reconcile.Reconciler
	private *httptest.Server
}

func newM7Env(t *testing.T, policy job.Policy) *m7Env {
	t.Helper()
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 500 * time.Millisecond,
		LeaseTTLS: 300, HeartbeatIntervalS: 30, Policy: policy,
	}, "m7")
	fake := gh.NewFake()
	sender := project.New(st, fake, clk, srv.Broker())
	sender.WithHistory(fakeMergeHistory{}, "main") // self-merge requires a mirror to pin+re-verify
	rec := reconcile.New(st, fake, clk, srv.Broker())

	private := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(private.Close)
	return &m7Env{st: st, fake: fake, clk: clk, srv: srv, sender: sender, rec: rec, private: private}
}

// drain runs the project-OUT sender until the outbox is empty.
func (e *m7Env) drain(t *testing.T, ctx context.Context) int {
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

func registerCaps(t *testing.T, ctx context.Context, url, id, family string, caps []string) *client.Client {
	t.Helper()
	c := client.New(url)
	if _, err := c.Register(ctx, client.Registration{
		WorkerID: "wk-" + id, Identity: id, Host: "test", Capabilities: caps,
	}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	return c
}

// ── TestM7_SpecFlowToMaterializedIssue: the spec-flow happy path (§11). ──
// A spec doc -> spec job commits spec.md + a BLAKE3 hash; a DISTINCT-LENS reviewer
// signs off (an author-lens reviewer is refused); sign-off materializes a real
// issue via project-OUT, audited once per key.
func TestM7_SpecFlowToMaterializedIssue(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()
	url := e.private.URL

	// seed the first Flowbee job: a spec_author job (chat is the lineage root).
	specJob := "spec-1"
	if _, err := e.st.SeedSpecJob(ctx, store.SeedSpecParams{
		ID: specJob, ChatRef: "chat-c5521", AuthorLens: "product_speccer", Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed spec job: %v", err)
	}

	// the spec_author leases, drafts, submits prose. Flowbee commits + hashes it.
	author := registerCaps(t, ctx, url, "author-amy", "opus", []string{"role:spec_author", "model_family:opus"})
	ag, ok, err := author.Lease(ctx, "author-amy", "opus", string(job.RoleSpecAuthor))
	if err != nil || !ok || ag.JobID != specJob {
		t.Fatalf("author lease ok=%v err=%v job=%s", ok, err, ag.JobID)
	}
	const specV1 = "# Feature X\n\nBuild the merge-queue path.\n"
	hash, vers, st, err := author.SpecSubmit(ctx, specJob, ag.LeaseEpoch, specV1, 1)
	if err != nil || st != http.StatusOK {
		t.Fatalf("spec submit st=%d err=%v", st, err)
	}
	// Flowbee — not the worker — owns the hash: it equals the BLAKE3 of the bytes.
	if hash != spec.ContentHash([]byte(specV1)) {
		t.Fatalf("Flowbee must compute the BLAKE3 content hash, got %q", hash)
	}
	if vers != 1 {
		t.Fatalf("spec version=%d want 1", vers)
	}
	j, _ := e.st.GetJob(ctx, specJob)
	if j.State != job.StateSpecReview || j.SpecContentHash != hash {
		t.Fatalf("after submit state=%s hash=%s want spec_review + bound hash", j.State, j.SpecContentHash)
	}

	// an AUTHOR-LENS reviewer is refused the gate (§5.5 distinct-lens term, I-10).
	sameLens := registerCaps(t, ctx, url, "rev-clone", "codex", []string{"role:spec_reviewer", "model_family:codex"})
	if g, ok, _ := sameLens.LeaseWithLens(ctx, "rev-clone", "codex", string(job.RoleSpecReviewer), "product_speccer"); ok {
		t.Fatalf("a reviewer with the AUTHOR lens must not win the spec_review lease, got %s", g.JobID)
	}

	// a DISTINCT-LENS reviewer leases the gate, judges the EXACT hash, signs off.
	reviewer := registerCaps(t, ctx, url, "rev-rob", "codex", []string{"role:spec_reviewer", "model_family:codex"})
	rg, ok, err := reviewer.LeaseWithLens(ctx, "rev-rob", "codex", string(job.RoleSpecReviewer), "staff_engineer")
	if err != nil || !ok || rg.JobID != specJob {
		t.Fatalf("reviewer lease ok=%v err=%v job=%s", ok, err, rg.JobID)
	}
	if rg.SpecContentHash != hash {
		t.Fatalf("spec_review lease must carry the content hash to bind: got %q", rg.SpecContentHash)
	}
	resp, code, err := reviewer.SpecReview(ctx, specJob, rg.LeaseEpoch, "sr-1", "signed_off", hash, true, true)
	if err != nil || code != http.StatusOK {
		t.Fatalf("spec review code=%d err=%v", code, err)
	}
	if !resp.Minted || resp.JobState != string(job.StateDone) {
		t.Fatalf("a signed-off spec must mint + reach done, got %+v", resp)
	}

	// the minted sign-off is content-hash-bound + tamper-evident, never a worker report.
	j, _ = e.st.GetJob(ctx, specJob)
	if j.SpecSignoff == nil || !j.SpecSignoff.TamperEvident || j.SpecSignoff.Provenance != "minted" {
		t.Fatalf("spec sign-off not tamper-evident/minted: %+v", j.SpecSignoff)
	}
	if !j.SpecSignoff.Verify(hash) {
		t.Fatalf("sign-off must verify against the bound content hash")
	}

	// sign-off MATERIALIZES a real issue (project-OUT drains the issues.create row).
	if e.drain(t, ctx) != 1 {
		t.Fatalf("exactly one project-OUT action (issues.create) expected")
	}
	j, _ = e.st.GetJob(ctx, specJob)
	if j.IssueNum == 0 {
		t.Fatalf("sign-off must materialize a real issue (stamped number), got %d", j.IssueNum)
	}
	if len(e.fake.Issues()) != 1 {
		t.Fatalf("exactly one GitHub issue materialized, got %d", len(e.fake.Issues()))
	}

	// the audit log records the materialize action ONCE, keyed (job, action, hash).
	audit, _ := e.st.AuditLog(ctx, specJob)
	if n := countAction(audit, store.ActionCreateIssue); n != 1 {
		t.Fatalf("issues.create must appear exactly once in the audit log, got %d", n)
	}

	// a re-drain is a no-op (idempotent): no second issue, no second audit row.
	if e.drain(t, ctx) != 0 {
		t.Fatalf("re-drain must be a no-op")
	}
	if len(e.fake.Issues()) != 1 {
		t.Fatalf("re-drain materialized a SECOND issue")
	}

	// determinism: Fold(events) reproduces the spec-flow projection (the spec hash,
	// the minted sign-off, the materialized issue number) byte-for-byte.
	evs, _ := e.st.LoadEvents(ctx, specJob)
	folded, _ := ledger.Fold(evs)
	j, _ = e.st.GetJob(ctx, specJob)
	if folded.State != j.State || folded.SpecContentHash != j.SpecContentHash || folded.IssueNum != j.IssueNum {
		t.Fatalf("Fold != projection: fold={%s %s issue=%d} proj={%s %s issue=%d}",
			folded.State, folded.SpecContentHash, folded.IssueNum, j.State, j.SpecContentHash, j.IssueNum)
	}
	if folded.SpecSignoff == nil || j.SpecSignoff == nil ||
		folded.SpecSignoff.IntegrityHash != j.SpecSignoff.IntegrityHash {
		t.Fatalf("Fold != projection for the spec sign-off")
	}
}

// ── TestM7_SpecEditSupersedesSignoff: editing the spec voids the prior sign-off ──
// (§11.5). A sign-off bound to hash H1 is mechanically dead the instant the spec
// bytes change to H2; the gate re-arms against the new bytes.
func TestM7_SpecEditSupersedesSignoff(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()
	url := e.private.URL

	specJob := "spec-edit"
	if _, err := e.st.SeedSpecJob(ctx, store.SeedSpecParams{
		ID: specJob, ChatRef: "chat-x", AuthorLens: "product_speccer", Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	author := registerCaps(t, ctx, url, "auth2", "opus", []string{"role:spec_author", "model_family:opus"})
	ag, _, _ := author.Lease(ctx, "auth2", "opus", string(job.RoleSpecAuthor))
	const v1 = "# v1\nspec body one\n"
	h1, _, _, err := author.SpecSubmit(ctx, specJob, ag.LeaseEpoch, v1, 1)
	if err != nil {
		t.Fatalf("submit v1: %v", err)
	}

	// case A: the reviewer judges a STALE hash (the spec already advanced). The gate
	// rejects the claim as SUPERSEDED — a sign-off bound to old bytes never mints.
	reviewer := registerCaps(t, ctx, url, "rev2", "codex", []string{"role:spec_reviewer", "model_family:codex"})
	rg, _, _ := reviewer.LeaseWithLens(ctx, "rev2", "codex", string(job.RoleSpecReviewer), "staff_engineer")
	staleResp, _, err := reviewer.SpecReview(ctx, specJob, rg.LeaseEpoch, "stale", "signed_off", "blake3:stale-hash", true, true)
	if err != nil {
		t.Fatalf("stale review: %v", err)
	}
	if staleResp.Minted {
		t.Fatalf("a sign-off bound to a STALE hash must NOT mint (§11.5)")
	}
	if !staleResp.Superseded {
		t.Fatalf("a stale-hash claim must be rejected as superseded, got %+v", staleResp)
	}

	// re-author the gate (back to spec_authoring): re-lease, sign off the REAL hash.
	ag2, ok, _ := author.Lease(ctx, "auth2", "opus", string(job.RoleSpecAuthor))
	if !ok {
		t.Fatalf("re-author lease failed")
	}
	// submit the SAME v1 bytes again so the hash is current (a no-op edit re-binds).
	h1b, _, _, _ := author.SpecSubmit(ctx, specJob, ag2.LeaseEpoch, v1, 2)
	if h1b != h1 {
		t.Fatalf("identical bytes must hash identically: %q vs %q", h1, h1b)
	}
	rg2, _, _ := reviewer.LeaseWithLens(ctx, "rev2", "codex", string(job.RoleSpecReviewer), "staff_engineer")
	good, _, _ := reviewer.SpecReview(ctx, specJob, rg2.LeaseEpoch, "good", "signed_off", h1, true, true)
	if !good.Minted {
		t.Fatalf("a sign-off bound to the CURRENT hash must mint, got %+v", good)
	}
	j, _ := e.st.GetJob(ctx, specJob)
	if j.SpecSignoff == nil || !j.SpecSignoff.Verify(h1) {
		t.Fatalf("sign-off must verify against h1")
	}

	// case B: a human edits the spec AFTER sign-off (hash moves H1 -> H2). The
	// sign-off is mechanically SUPERSEDED — it no longer verifies against the new
	// bytes, and the gate re-arms (§11.5).
	const v2 = "# v2\nspec body TWO (edited)\n"
	h2 := spec.ContentHash([]byte(v2))
	if err := e.st.EditSpec(ctx, specJob, h2, 3, e.clk.Now()); err != nil {
		t.Fatalf("edit spec: %v", err)
	}
	j, _ = e.st.GetJob(ctx, specJob)
	if j.SpecSignoff != nil {
		t.Fatalf("the prior sign-off must be voided by the edit (§11.5)")
	}
	if j.State != job.StateSpecReview || j.SpecContentHash != h2 {
		t.Fatalf("the gate must re-arm against the new bytes: state=%s hash=%s", j.State, j.SpecContentHash)
	}
	// the OLD sign-off (had we kept it) would no longer verify against H2 — proven
	// by minting it fresh and checking it fails the new binding.
	if good.Minted {
		old := job.MintSpecSignoff(h1, 2, "staff_engineer")
		if old.Verify(h2) {
			t.Fatalf("an H1-bound sign-off must NOT verify against H2 (mechanical supersession)")
		}
	}
}

// ── TestM7_BuildPatchToOpenPRStampToMergeToDone: the build half (§7.3, §8.5). ──
// A builder patch -> Flowbee opens the PR and stamps #; the reviewer approves ->
// handoff (Branch A); Flowbee enqueues the merge; a human-merge fact reconciles to
// done. Every GitHub action appears once in the audit log keyed (job, action, sha).
func TestM7_BuildPatchToOpenPRStampToMergeToDone(t *testing.T) {
	e := newM7Env(t, job.Policy{}) // Branch A: handoff to human
	ctx := context.Background()
	url := e.private.URL

	buildJob := "build-1"
	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: buildJob, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base-sha-0",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed build: %v", err)
	}

	// the eng_worker builds, pushes its epoch ref, posts the result (NO pr field).
	builder := registerCaps(t, ctx, url, "bob", "codex", []string{"role:eng_worker", "model_family:codex"})
	bg, ok, err := builder.Lease(ctx, "bob", "codex", "")
	if err != nil || !ok || bg.JobID != buildJob {
		t.Fatalf("builder lease ok=%v err=%v", ok, err)
	}
	if _, _, err := builder.Result(ctx, buildJob, bg.LeaseEpoch, "build-1",
		map[string]any{"kind": "patch", "base_sha": "base-sha-0", "pushed_ref": "refs/flowbee/build-1/epoch-1"}); err != nil {
		t.Fatalf("builder result: %v", err)
	}

	// the canonical PR-open trigger (§7.3): Flowbee enqueues pulls.create at the
	// promoted head, drains it, and STAMPS the GitHub-returned number. The worker
	// never supplied a PR field.
	headSHA := "head-sha-1"
	enq, err := e.st.EnqueuePROpen(ctx, buildJob, headSHA, "main")
	if err != nil || !enq {
		t.Fatalf("enqueue PR-open enq=%v err=%v", enq, err)
	}
	if e.drain(t, ctx) != 1 {
		t.Fatalf("exactly one project-OUT action (pulls.create) expected")
	}
	prNum, _ := e.st.JobPR(ctx, buildJob)
	if prNum == 0 {
		t.Fatalf("Flowbee must open the PR and stamp the number")
	}
	if _, ok := e.fake.PRState(prNum); !ok {
		t.Fatalf("the opened PR must exist in GitHub (the fake), #%d", prNum)
	}

	// the reviewer leases the gate; reconcile-IN supplies green Domain-B facts at the
	// stamped PR. The reviewer approves; Branch A forces handoff.
	// reconcile-IN supplies green facts BEFORE the reviewer leases (the review gate
	// is offered only once CI is green, matching production ordering).
	if err := e.st.UpsertDomainBFacts(ctx, buildJob, job.DomainBFacts{
		PRExists: true, PRNumber: prNum, HeadSHA: headSHA, BaseSHA: "base-sha-0", CIGreen: true,
	}); err != nil {
		t.Fatalf("reconcile facts: %v", err)
	}
	e.fake.SetPR(gh.PullRequest{
		Number: prNum, HeadRefOid: headSHA, BaseRefOid: "base-sha-0",
		CIRollup: gh.CISuccess, PassedChecks: []string{"acceptance"},
	})
	reviewer := client.New(url)
	if _, err := reviewer.Register(ctx, client.Registration{
		WorkerID: "wk-rev", Identity: "rev", Host: "t",
		Capabilities: []string{"role:code_reviewer", "model_family:opus"},
	}); err != nil {
		t.Fatalf("reviewer register: %v", err)
	}
	rg, ok, err := reviewer.Lease(ctx, "rev", "opus", string(job.RoleCodeReviewer))
	if err != nil || !ok || rg.JobID != buildJob {
		t.Fatalf("reviewer lease ok=%v err=%v", ok, err)
	}
	// the eng_worker may NOT review its own build (anti-affinity, I-10).
	if g, ok, _ := builder.Lease(ctx, "bob", "codex", string(job.RoleCodeReviewer)); ok {
		t.Fatalf("the builder must not win its own code_review, got %s", g.JobID)
	}
	rv, code, err := reviewer.Review(ctx, buildJob, rg.LeaseEpoch, "rev-1", "approved", "handoff", "", "")
	if err != nil || code != http.StatusOK {
		t.Fatalf("review code=%d err=%v", code, err)
	}
	if !rv.Minted || rv.JobState != string(job.StateMergeHandoff) {
		t.Fatalf("approved (Branch A) must mint + reach merge_handoff, got %+v", rv)
	}

	// Flowbee enqueues the merge (both arms physically merge via the queue, §5.4).
	menq, err := e.st.EnqueueMergeForJob(ctx, buildJob, e.clk.Now())
	if err != nil || !menq {
		t.Fatalf("enqueue merge enq=%v err=%v", menq, err)
	}
	e.drain(t, ctx)
	if eq := e.fake.Enqueued(); len(eq) != 1 || eq[0] != prNum {
		t.Fatalf("the PR must be enqueued to the merge queue once, got %v", eq)
	}

	// the HUMAN merges on GitHub; reconcile-IN observes the terminal fact and flips
	// the job to done (I-3, §3.4). The merge is never a Flowbee-fabricated fact.
	e.fake.SetPR(gh.PullRequest{
		Number: prNum, HeadRefOid: headSHA, BaseRefOid: "base-sha-0",
		Merged: true, MergeCommit: "merge-commit-abc", CIRollup: gh.CISuccess,
		UpdatedAt: time.Unix(1_700_001_000, 0),
	})
	if _, err := e.rec.Sweep(ctx); err != nil {
		t.Fatalf("reconcile sweep: %v", err)
	}
	j, _ := e.st.GetJob(ctx, buildJob)
	if j.State != job.StateDone {
		t.Fatalf("the human-merge fact must reconcile to done, got %s", j.State)
	}

	// the merge enqueued the F11 post-merge history write (docs/history/<id>.md + the
	// TOC); drain it so the audit baseline below includes that one-time action and the
	// idempotent-re-drain assertion measures only DUPLICATES.
	e.drain(t, ctx)

	// every GitHub action appears ONCE in the audit log keyed (job, action, head_sha).
	audit, _ := e.st.AuditLog(ctx, buildJob)
	if countAction(audit, store.ActionOpenPR) != 1 {
		t.Fatalf("pulls.create must be audited exactly once")
	}
	if countAction(audit, store.ActionEnqueueMerge) != 1 {
		t.Fatalf("mergeQueue.enqueue must be audited exactly once")
	}
	// re-draining everything cannot duplicate any audited action (idempotent key).
	_, _ = e.st.EnqueuePROpen(ctx, buildJob, headSHA, "main")
	_, _ = e.st.EnqueueMergeForJob(ctx, buildJob, e.clk.Now())
	e.drain(t, ctx)
	audit2, _ := e.st.AuditLog(ctx, buildJob)
	if len(audit2) != len(audit) {
		t.Fatalf("re-enqueue + re-drain duplicated an audited action: %d -> %d", len(audit), len(audit2))
	}
}

// ── TestM7_AdoptImportsQuiescentAndUntouched: ADOPT mode (§12.7, I-16). ──
// Pre-existing PRs import quiescent (reconciled, NOT scheduled) and project-OUT is
// SUPPRESSED on them — Flowbee never seizes human-owned in-flight work. An opt-in
// PR (watermark) enters the normal DAG.
func TestM7_AdoptImportsQuiescentAndUntouched(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()

	watermark := time.Unix(1_700_000_500, 0)
	// PR 11: pre-existing, BEFORE the watermark -> stays quiescent (human-owned).
	// PR 22: created AFTER the watermark -> opted in (start-fresh default).
	e.fake.SetPR(gh.PullRequest{Number: 11, HeadRefOid: "h11", BaseRefOid: "main", CIRollup: gh.CIFailure, UpdatedAt: time.Unix(1_700_000_100, 0)})
	e.fake.SetPR(gh.PullRequest{Number: 22, HeadRefOid: "h22", BaseRefOid: "main", CIRollup: gh.CISuccess, UpdatedAt: time.Unix(1_700_000_900, 0)})

	snap, err := e.fake.BoardSweep(ctx)
	if err != nil {
		t.Fatalf("board sweep: %v", err)
	}
	adopted, err := e.st.AdoptSweep(ctx, snap, watermark, e.clk.Now())
	if err != nil {
		t.Fatalf("adopt sweep: %v", err)
	}
	if len(adopted) != 2 {
		t.Fatalf("both PRs must be imported as jobs, got %d", len(adopted))
	}

	// PR 11 is QUIESCENT: reconciled (full Domain-B facts) but NOT scheduled.
	q, _ := e.st.GetJob(ctx, "adopt-pr-11")
	if q.State != job.StateQuiescent {
		t.Fatalf("a pre-watermark PR must import quiescent, got %s", q.State)
	}
	f, ok, _ := store.DBFactSource{DB: e.st.DB}.Facts(ctx, "adopt-pr-11")
	if !ok || f.HeadSHA != "h11" || !f.PRExists {
		t.Fatalf("a quiescent job must still be reconciled (Domain-B): %+v", f)
	}
	isQ, _ := e.st.IsQuiescent(ctx, "adopt-pr-11")
	if !isQ {
		t.Fatalf("PR 11 must be flagged quiescent")
	}

	// PR 22 opted in (watermark): it entered the normal DAG (review_pending).
	in, _ := e.st.GetJob(ctx, "adopt-pr-22")
	if in.State != job.StateReviewPending {
		t.Fatalf("a post-watermark PR must opt in to the DAG, got %s", in.State)
	}

	// project-OUT is SUPPRESSED on the quiescent job (§8.2.3): even with a rendering
	// enqueued, draining writes NOTHING to GitHub and produces NO audit row.
	if err := e.st.EnqueueOutbox(ctx, store.OutboxRow{
		JobID: "adopt-pr-11", Action: store.ActionSetLabels, HeadSHA: "h11",
		Payload: `{"labels":["flowbee:building"]}`,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	before := len(e.fake.Calls())
	e.drain(t, ctx)
	// no write call beyond the prior BoardSweep reached GitHub for the quiescent job.
	for _, c := range e.fake.Calls()[before:] {
		t.Fatalf("project-OUT must be suppressed on a quiescent job, but called %s", c)
	}
	audit, _ := e.st.AuditLog(ctx, "adopt-pr-11")
	if len(audit) != 0 {
		t.Fatalf("a quiescent job must have NO audited actions (untouched), got %d", len(audit))
	}
	if labels := e.fake.Labels(11); len(labels) != 0 {
		t.Fatalf("the human's PR must be untouched (no labels set), got %v", labels)
	}

	// the operator opts PR 11 in deliberately -> it enters the DAG; now project-OUT
	// renders it (no longer suppressed).
	if err := e.st.OptIn(ctx, "adopt-pr-11", e.clk.Now()); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	if j, _ := e.st.GetJob(ctx, "adopt-pr-11"); j.State != job.StateReviewPending {
		t.Fatalf("opt-in must move the job into the DAG, got %s", j.State)
	}
}

// ── TestM7_OutboxParksOnRetryAfter: §8.2.4 — a Retry-After parks the whole outbox.
func TestM7_OutboxParksOnRetryAfter(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()

	if _, err := e.st.SeedSpecJob(ctx, store.SeedSpecParams{ID: "sp", ChatRef: "c", Now: e.clk.Now()}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// drive a sign-off directly to enqueue a materialize_issues row.
	hash := spec.ContentHash([]byte("body"))
	if _, err := e.st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='spec_review', spec_content_hash=?, reviewer_lens='staff_engineer' WHERE id='sp'`, hash); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := e.st.SpecReviewResult(ctx, store.SpecReviewResultParams{
		JobID: "sp", Epoch: 0, Claim: job.VerdictSignedOff, BindsTo: hash,
		MeetsStyle: true, MeetsRequirements: true, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("spec signoff: %v", err)
	}

	// the next write hits a secondary rate limit: the sender parks the WHOLE outbox.
	e.fake.FailNextWriteWithRetryAfter(60 * time.Second)
	if n, err := e.sender.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("a Retry-After must park the outbox (0 sent), got n=%d err=%v", n, err)
	}
	if len(e.fake.Issues()) != 0 {
		t.Fatalf("no issue may be created while parked")
	}
	// while parked, a re-drain still does nothing.
	if n, _ := e.sender.DrainOnce(ctx); n != 0 {
		t.Fatalf("the outbox must stay parked")
	}
	// once the park expires (clock advances past it), the drain proceeds.
	e.clk.Advance(61 * time.Second)
	if n := e.drain(t, ctx); n != 1 {
		t.Fatalf("after the park expires the row must drain, got %d", n)
	}
	if len(e.fake.Issues()) != 1 {
		t.Fatalf("the parked issue must materialize after the park")
	}
}

// ── TestM7_BranchProtectionAssertion: I-8 startup backstop (§9.6). ──
func TestM7_BranchProtectionAssertion(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()

	// no protection configured -> the startup assertion fails closed.
	if err := project.AssertBranchProtection(ctx, e.fake, "main"); err == nil {
		t.Fatalf("an unprotected branch must fail the I-8 assertion")
	}
	// force-push allowed -> still fails.
	e.fake.SetBranchProtection("main", gh.Protection{NoForcePush: false, RequireDistinctReviewer: true})
	if err := project.AssertBranchProtection(ctx, e.fake, "main"); err == nil {
		t.Fatalf("a force-pushable branch must fail the I-8 assertion")
	}
	// no distinct-reviewer requirement -> still fails.
	e.fake.SetBranchProtection("main", gh.Protection{NoForcePush: true, RequireDistinctReviewer: false})
	if err := project.AssertBranchProtection(ctx, e.fake, "main"); err == nil {
		t.Fatalf("a branch without required distinct review must fail the I-8 assertion")
	}
	// a correctly-protected branch passes.
	e.fake.SetBranchProtection("main", gh.Protection{
		NoForcePush: true, RequirePR: true, RequiredReviews: 1, RequireDistinctReviewer: true,
	})
	if err := project.AssertBranchProtection(ctx, e.fake, "main"); err != nil {
		t.Fatalf("a correctly-protected branch must pass: %v", err)
	}
}

// countAction counts audit rows for a given action.
func countAction(rows []store.AuditRow, action string) int {
	n := 0
	for _, r := range rows {
		if r.Action == action {
			n++
		}
	}
	return n
}
