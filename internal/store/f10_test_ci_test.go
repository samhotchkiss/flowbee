package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

// TestF10ArchLotteryRoutesTestJobToArmWorkerOnly is the F10 arch-lottery
// acceptance: an arm64 `test` job (capability-matched on DIFF-DERIVED constraints)
// routes ONLY to an arm64-capable worker and NEVER to an x86 worker. It exercises
// the REAL attestation path (worker.Registry attests arch:* against the worker's
// handshake) + the REAL store claim path (ClaimReadyJob enforces the required caps
// against the attested set). No fakes are stubbed for the routing decision.
func TestF10ArchLotteryRoutesTestJobToArmWorkerOnly(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := worker.NewRegistry(st, 300, 30, worker.OpenAllowlist())
	now := time.Unix(1000, 0)

	// register an arm64 box and an x86 box. The registry ATTESTS arch:* against the
	// submitted handshake — an x86 box claiming arch:arm64 would have it dropped.
	armReg, err := reg.Register(ctx, worker.Registration{
		WorkerID: "arm-box", Identity: "arm-tester", Host: "h1",
		Arch: "arm64", OS: "linux",
		Capabilities: []string{"role:tester", "arch:arm64", "os:linux"},
	}, now)
	if err != nil {
		t.Fatalf("register arm: %v", err)
	}
	x86Reg, err := reg.Register(ctx, worker.Registration{
		WorkerID: "x86-box", Identity: "x86-tester", Host: "h2",
		Arch: "x86_64", OS: "linux",
		// the x86 box even tries to CLAIM arch:arm64 — attestation must drop it.
		Capabilities: []string{"role:tester", "arch:arm64", "arch:x86_64", "os:linux"},
	}, now)
	if err != nil {
		t.Fatalf("register x86: %v", err)
	}

	// the x86 box's lie must have been dropped: it attests x86_64, never arm64.
	if hasCap(x86Reg.AttestedCapabilities, "arch:arm64") {
		t.Fatalf("x86 box must NOT attest arch:arm64 (handshake gate): %v", x86Reg.AttestedCapabilities)
	}
	if !hasCap(armReg.AttestedCapabilities, "arch:arm64") {
		t.Fatalf("arm box must attest arch:arm64: %v", armReg.AttestedCapabilities)
	}

	// DERIVE the test job's required caps from an arm64 matrix (the arch-lottery fix).
	req := job.DeriveTestConstraints(job.TestMatrix{Arch: "arm64", OS: "linux"}, nil)
	if !hasCap(req, "arch:arm64") {
		t.Fatalf("derived test constraints missing arch:arm64: %v", req)
	}

	// seed the arm64 `test` job carrying those required caps.
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "arm-test", Kind: job.KindTest, Flow: "test", Stage: "test",
		Role: job.RoleTester, BaseSHA: "b1", RequiredCapabilities: req, Now: now,
	}); err != nil {
		t.Fatalf("seed test job: %v", err)
	}

	// the x86 worker tries first — it MUST lose the race (no arch:arm64 attested).
	x86Attested, _ := reg.AttestedFor(ctx, "x86-tester", now)
	_, err = st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "arm-test", LeaseID: "l-x86", Identity: "x86-tester", ModelFamily: "codex",
		Role: job.RoleTester, Attested: x86Attested, TTL: time.Minute, Now: now,
	})
	if err == nil {
		t.Fatal("x86 worker must NOT win the arm64 test job (arch lottery fix)")
	}
	if j, _ := st.GetJob(ctx, "arm-test"); j.State != job.StateReady {
		t.Fatalf("test job state=%s want still ready after x86 lost", j.State)
	}

	// the arm64 worker wins.
	armAttested, _ := reg.AttestedFor(ctx, "arm-tester", now)
	ls, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: "arm-test", LeaseID: "l-arm", Identity: "arm-tester", ModelFamily: "codex",
		Role: job.RoleTester, Attested: armAttested, TTL: time.Minute, Now: now,
	})
	if err != nil || ls == nil {
		t.Fatalf("arm64 worker should win the arm64 test job: %v", err)
	}
}

// TestFlowbeeTestCICannotAuthorizeGitHubVisiblePRHead locks the #272/#273
// authority boundary: PR review/merge authorization is bound to the reconciled
// GitHub-visible head and its required checks. Flowbee test facts may be recorded
// for audit, but they cannot substitute for GitHub-required CI at the current PR
// head — not from the current hidden test head, a prior head, or the base head.
func TestFlowbeeTestCICannotAuthorizeGitHubVisiblePRHead(t *testing.T) {
	ctx := context.Background()
	policy := job.Policy{} // handoff posture; CI-greenness is what we are testing

	for _, c := range []struct {
		name    string
		factSHA string
	}{
		{name: "hidden_current_head", factSHA: "head-A"},
		{name: "prior_head", factSHA: "old-head-A"},
		{name: "base_head", factSHA: "base-A"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := testutil.NewStore(t)
			src := store.DBFactSource{DB: st.DB}
			driveToCodeReview(t, st, "buildA-"+c.name, "head-A", "base-A")

			mustFacts(t, st, "buildA-"+c.name, job.DomainBFacts{
				PRExists: true, PRNumber: 1, HeadSHA: "head-A", BaseSHA: "base-A",
				CIGreen: false, Merged: false,
			})

			if err := st.RecordTestJobCI(ctx, "buildA-"+c.name, c.factSHA, "arm-test", true, time.Unix(2000, 0)); err != nil {
				t.Fatalf("record test ci: %v", err)
			}

			resp, err := st.ReviewResult(ctx, src, policy, store.ReviewResultParams{
				JobID: "buildA-" + c.name, Epoch: epochOf(t, st, "buildA-"+c.name),
				Claim: job.VerdictApproved, Disposition: job.DispositionHandoff,
				Now: time.Unix(3000, 0),
			})
			if err != nil {
				t.Fatalf("review result: %v", err)
			}
			if resp.Minted || resp.JobState == string(job.StateMergeable) {
				t.Fatalf("Flowbee test fact at %s must not authorize GitHub-visible head; resp=%+v", c.factSHA, resp)
			}
		})
	}

	t.Run("reconciled_actions_green_alone_satisfies", func(t *testing.T) {
		st := testutil.NewStore(t)
		src := store.DBFactSource{DB: st.DB}
		driveToCodeReview(t, st, "buildC", "head-C", "base-C")
		mustFacts(t, st, "buildC", job.DomainBFacts{
			PRExists: true, PRNumber: 3, HeadSHA: "head-C", BaseSHA: "base-C",
			CIGreen: true, Merged: false,
		})
		resp, err := st.ReviewResult(ctx, src, policy, store.ReviewResultParams{
			JobID: "buildC", Epoch: epochOf(t, st, "buildC"),
			Claim: job.VerdictApproved, Disposition: job.DispositionHandoff,
			Now: time.Unix(3000, 0),
		})
		if err != nil {
			t.Fatalf("review result: %v", err)
		}
		if !resp.Minted {
			t.Fatal("reconciled Actions green must still satisfy the gate on its own")
		}
	})
}

// ── helpers ──

func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// driveToCodeReview seeds a build and drives it leased -> review_pending ->
// code_review so ReviewResult can run the gate over it.
func driveToCodeReview(t *testing.T, st *store.Store, id, head, base string) {
	t.Helper()
	ctx := context.Background()
	now := time.Unix(1000, 0)
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: base, Now: now,
	}); err != nil {
		t.Fatalf("seed build %s: %v", id, err)
	}
	ls, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: id, LeaseID: "l-" + id, Identity: "builder-" + id, ModelFamily: "codex",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker", "model_family:codex"},
		TTL: time.Minute, Now: now,
	})
	if err != nil {
		t.Fatalf("claim build %s: %v", id, err)
	}
	if _, err := st.Result(ctx, store.ResultParams{
		JobID: id, Epoch: ls.Epoch, PushedSHA: head, Now: time.Unix(1500, 0),
	}); err != nil {
		t.Fatalf("build result %s: %v", id, err)
	}
	// bind the head sha the gate will judge.
	if err := st.SetReconciledFacts(ctx, id, store.ReconciledPR{
		Number: 0, HeadSHA: head, BaseSHA: base,
	}); err != nil {
		t.Fatalf("set head %s: %v", id, err)
	}
	if _, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: id, LeaseID: "rl-" + id, Identity: "reviewer-" + id, ModelFamily: "opus",
		Attested: []string{"role:code_reviewer", "model_family:opus"},
		TTL:      time.Minute, Now: time.Unix(1600, 0),
	}); err != nil {
		t.Fatalf("claim review %s: %v", id, err)
	}
}

func mustFacts(t *testing.T, st *store.Store, id string, f job.DomainBFacts) {
	t.Helper()
	if err := st.UpsertDomainBFacts(context.Background(), id, f); err != nil {
		t.Fatalf("upsert facts %s: %v", id, err)
	}
}

func epochOf(t *testing.T, st *store.Store, id string) int {
	t.Helper()
	j, err := st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("get job %s: %v", id, err)
	}
	return j.LeaseEpoch
}
