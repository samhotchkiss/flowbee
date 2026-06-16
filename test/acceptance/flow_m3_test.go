// M3 acceptance: the flow engine + build-flow code_review GATE, proven
// end-to-end over the real HTTP surface against a real SQLite store, GitHub
// stubbed (a DB-backed FactSource the test seeds in place of reconcile-IN).
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - a seeded build job drives building -> review_pending -> code_review across
//     DISTINCT builder + reviewer stubs;
//   - changes_requested bounces (code_review -> building) and increments bounces;
//   - max_bounces -> needs_human;
//   - an approved verdict + green stubbed FactSource MINTS a SHA-bound,
//     tamper-evident verdict (never from worker status) -> mergeable -> merge_handoff;
//   - the §5.6 neutrality lint FAILS the build on a planted provider literal.
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
	"github.com/samhotchkiss/flowbee/internal/flow"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func newM3Server(st *store.Store, clk clock.Clock, policy job.Policy) *api.Server {
	return api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: time.Second,
		LeaseTTLS: 300, HeartbeatIntervalS: 30, Policy: policy,
	}, "m3")
}

// driveToCodeReview seeds a build job, has a builder produce a result
// (building -> review_pending), then a DISTINCT reviewer leases the gate stage
// (review_pending -> code_review). Returns the reviewer client + its lease grant.
func driveToCodeReview(t *testing.T, ctx context.Context, st *store.Store, url, jobID string) (*client.Client, client.LeaseGrant) {
	t.Helper()

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base1",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// builder leases + produces a result -> review_pending.
	builder := registerWorker(t, ctx, url, "builder-alice", "codex")
	bg, ok, err := builder.Lease(ctx, "builder-alice", "codex", "")
	if err != nil || !ok || bg.JobID != jobID {
		t.Fatalf("builder lease ok=%v err=%v job=%s", ok, err, bg.JobID)
	}
	if _, _, err := builder.Result(ctx, jobID, bg.LeaseEpoch, "build-1", map[string]any{"kind": "patch", "base_sha": "base1"}); err != nil {
		t.Fatalf("builder result: %v", err)
	}
	if j, _ := st.GetJob(ctx, jobID); j.State != job.StateReviewPending {
		t.Fatalf("after build result state=%s want review_pending", j.State)
	}

	// a DISTINCT reviewer (different identity AND model_family) leases the gate.
	reviewer := client.New(url)
	if _, err := reviewer.Register(ctx, client.Registration{
		WorkerID: "wk-reviewer-bob", Identity: "reviewer-bob", Host: "t",
		Capabilities: []string{"role:code_reviewer", "model_family:opus"},
	}); err != nil {
		t.Fatalf("reviewer register: %v", err)
	}
	rg, ok, err := reviewer.Lease(ctx, "reviewer-bob", "opus", string(job.RoleCodeReviewer))
	if err != nil || !ok {
		t.Fatalf("reviewer lease ok=%v err=%v", ok, err)
	}
	if rg.JobID != jobID || rg.Role != string(job.RoleCodeReviewer) {
		t.Fatalf("reviewer leased %s role=%s want %s code_reviewer", rg.JobID, rg.Role, jobID)
	}
	if j, _ := st.GetJob(ctx, jobID); j.State != job.StateCodeReview {
		t.Fatalf("after reviewer lease state=%s want code_review", j.State)
	}
	// the builder must NOT be able to lease its own review (distinct stubs only).
	if g, ok, _ := builder.Lease(ctx, "builder-alice", "codex", string(job.RoleCodeReviewer)); ok {
		t.Fatalf("eng_worker must not win the code_review lease, got %s", g.JobID)
	}
	return reviewer, rg
}

// TestM3ApprovedMintsVerdictAndHandsOff proves the happy path: green FactSource +
// approved claim MINTS a SHA-bound tamper-evident verdict (I-9) and reaches
// merge_handoff (Branch A: self_merge policy off).
func TestM3ApprovedMintsVerdictAndHandsOff(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(5000, 0))
	srv := newM3Server(st, clk, job.Policy{AllowSelfMerge: false})
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-approve"
	reviewer, rg := driveToCodeReview(t, ctx, st, ts.URL, jobID)

	// reconcile-IN (stubbed): green Domain-B facts for this job's PR.
	if err := st.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
		PRExists: true, PRNumber: 42, HeadSHA: "head-abc", BaseSHA: "base1", CIGreen: true,
	}); err != nil {
		t.Fatalf("seed facts: %v", err)
	}

	// the reviewer claims approved + self_merge — but policy is OFF, so the gate
	// mints a verdict bound to the reconciled SHA pair and forces handoff.
	resp, code, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "rev-1", "approved", "self_merge")
	if err != nil || code != http.StatusOK {
		t.Fatalf("review code=%d err=%v", code, err)
	}
	if !resp.Minted || resp.Verdict != "approved" {
		t.Fatalf("expected a minted approved verdict, got %+v", resp)
	}
	if resp.JobState != string(job.StateMergeHandoff) {
		t.Fatalf("approved (policy off) should reach merge_handoff, got %s", resp.JobState)
	}

	// the verdict in the projection is tamper-evident, reconciled, and SHA-bound.
	j, _ := st.GetJob(ctx, jobID)
	if j.Verdict == nil {
		t.Fatal("projection has no minted verdict")
	}
	if j.Verdict.Provenance != "reconciled" || !j.Verdict.TamperEvident {
		t.Fatalf("verdict not tamper-evident/reconciled: %+v", j.Verdict)
	}
	if !j.Verdict.Verify("head-abc", "base1") {
		t.Fatalf("verdict must verify against the reconciled SHA pair: %+v", j.Verdict)
	}
	// it must NOT verify against any other SHA (a move supersedes it, I-5).
	if j.Verdict.Verify("head-xyz", "base1") {
		t.Fatal("verdict must not verify against a moved head SHA")
	}

	// the ledger records the untrusted claim AND the minted verdict separately
	// (the verdict is NEVER taken from the worker status).
	evs, _ := st.LoadEvents(ctx, jobID)
	var sawClaim, sawMint bool
	for _, e := range evs {
		if e.Kind == ledger.KindVerdictClaim {
			sawClaim = true
		}
		if e.Kind == ledger.KindVerdictMinted {
			sawMint = true
			if e.Payload.Verdict == nil || e.Payload.Verdict.IntegrityHash == "" {
				t.Fatal("verdict_minted event missing the minted verdict")
			}
		}
	}
	if !sawClaim || !sawMint {
		t.Fatalf("ledger must record both the claim and the mint (claim=%v mint=%v)", sawClaim, sawMint)
	}
	// Fold(events) reproduces the gate-relevant projection (determinism).
	folded, _ := ledger.Fold(evs)
	if folded.State != j.State || folded.Bounces != j.Bounces ||
		folded.Verdict == nil || folded.Verdict.IntegrityHash != j.Verdict.IntegrityHash {
		t.Fatalf("Fold != projection:\n fold=%+v\n proj=%+v", folded.Verdict, j.Verdict)
	}
}

// TestM3HostileApprovalOverRedFactsNeverMints is the I-9 keystone: a worker that
// lies (claims approved) over a RED FactSource produces NO verdict — it bounces.
func TestM3HostileApprovalOverRedFactsNeverMints(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(5000, 0))
	srv := newM3Server(st, clk, job.Policy{})
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-hostile"
	reviewer, rg := driveToCodeReview(t, ctx, st, ts.URL, jobID)

	// reconcile-IN: PR exists but CI is RED.
	if err := st.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
		PRExists: true, PRNumber: 9, HeadSHA: "h", BaseSHA: "base1", CIGreen: false,
	}); err != nil {
		t.Fatal(err)
	}

	resp, code, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "rev-h", "approved", "handoff")
	if err != nil || code != http.StatusOK {
		t.Fatalf("review code=%d err=%v", code, err)
	}
	if resp.Minted {
		t.Fatalf("hostile approval over red CI must NOT mint a verdict (I-9): %+v", resp)
	}
	// a bounce re-arms the build stage (`ready`, re-leasable) — NOT `building`,
	// which is an active-lease state with no worker (§6.2.2 diagram).
	if resp.JobState != string(job.StateReady) {
		t.Fatalf("hostile approval should bounce to ready, got %s", resp.JobState)
	}
	j, _ := st.GetJob(ctx, jobID)
	if j.Verdict != nil {
		t.Fatalf("no verdict may exist after a refused approval: %+v", j.Verdict)
	}
	if j.Bounces != 1 {
		t.Fatalf("a refused approval counts as a bounce, got %d", j.Bounces)
	}
}

// TestM3ChangesRequestedBouncesAndExhausts proves the bounce loop: changes_requested
// bounces (code_review -> building) and increments bounces; the third hits
// max_bounces -> needs_human. attempts is NOT touched (distinct counter).
func TestM3ChangesRequestedBouncesAndExhausts(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(6000, 0))
	srv := newM3Server(st, clk, job.Policy{})
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-bounce"
	reviewer, rg := driveToCodeReview(t, ctx, st, ts.URL, jobID)

	// bounce #1: changes_requested -> ready (re-armed build stage), bounces=1.
	resp, _, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "cr-1", "changes_requested", "")
	if err != nil {
		t.Fatalf("review #1: %v", err)
	}
	if resp.JobState != string(job.StateReady) {
		t.Fatalf("changes_requested should bounce to ready, got %s", resp.JobState)
	}
	if j, _ := st.GetJob(ctx, jobID); j.Bounces != 1 || j.Attempts != 0 {
		t.Fatalf("after bounce #1 bounces=%d attempts=%d want 1/0", j.Bounces, j.Attempts)
	}

	// re-build (building -> review_pending) then re-review for bounce #2 and #3.
	for i := 2; i <= 3; i++ {
		rebuildToReviewPending(t, ctx, st, ts.URL, jobID, i)
		rv := rereview(t, ctx, st, ts.URL, jobID)
		resp, _, err := rv.cl.Review(ctx, jobID, rv.epoch, "", "changes_requested", "")
		if err != nil {
			t.Fatalf("review #%d: %v", i, err)
		}
		if i < 3 {
			if resp.JobState != string(job.StateReady) {
				t.Fatalf("bounce #%d should be ready, got %s", i, resp.JobState)
			}
			if j, _ := st.GetJob(ctx, jobID); j.Bounces != i {
				t.Fatalf("after bounce #%d bounces=%d", i, j.Bounces)
			}
		} else {
			// the third changes_requested reaches max_bounces -> needs_human.
			if resp.JobState != string(job.StateNeedsHuman) {
				t.Fatalf("max_bounces should reach needs_human, got %s", resp.JobState)
			}
		}
	}

	j, _ := st.GetJob(ctx, jobID)
	if j.State != job.StateNeedsHuman {
		t.Fatalf("final state=%s want needs_human", j.State)
	}
	if j.Bounces < 3 {
		t.Fatalf("final bounces=%d want >=3", j.Bounces)
	}
	if j.Attempts != 0 {
		t.Fatalf("bounces must not touch attempts, got attempts=%d", j.Attempts)
	}
}

// TestM3SelfMergeWhenPolicyOn proves the §5.4 branch point flips with THE ONE
// DECISION: with AllowSelfMerge ON and a self_merge disposition, an approval
// reaches `merging` (not handoff) — a pure policy flip, same code path.
func TestM3SelfMergeWhenPolicyOn(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(7000, 0))
	srv := newM3Server(st, clk, job.Policy{AllowSelfMerge: true})
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-selfmerge"
	reviewer, rg := driveToCodeReview(t, ctx, st, ts.URL, jobID)
	if err := st.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
		PRExists: true, PRNumber: 1, HeadSHA: "hh", BaseSHA: "base1", CIGreen: true,
	}); err != nil {
		t.Fatal(err)
	}
	resp, _, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "sm-1", "approved", "self_merge")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if !resp.Minted || resp.JobState != string(job.StateMerging) {
		t.Fatalf("policy-on self_merge should reach merging, got %+v", resp)
	}
}

// TestM3NeutralityLintFailsOnPlantedLiteral is the §5.6 DONE-WHEN at the
// acceptance level: a flow config with a planted provider literal in a control
// position FAILS to load (the build fails).
func TestM3NeutralityLintFailsOnPlantedLiteral(t *testing.T) {
	planted := []byte(`
roles:
  eng_worker: { requires: ["role:eng_worker", "model_family:*"], lens: { prompt_ref: lenses/eng_worker.md } }
flows:
  build:
    stages:
      build: { role: eng_worker }
      review: { role: eng_worker, when: "agent == 'codex'" }
`)
	if _, err := flow.Parse(planted); err == nil {
		t.Fatal("a planted provider literal must FAIL the flow load (§5.6)")
	}
	// and the shipped default config still lints clean.
	data, err := readDefaultFlows()
	if err != nil {
		t.Fatalf("read default flows: %v", err)
	}
	if _, err := flow.Parse(data); err != nil {
		t.Fatalf("default flows must lint clean: %v", err)
	}
}
