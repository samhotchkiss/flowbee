// Epic-lane Phase 4 acceptance: the criteria-driven merge gate (landed from the
// Phase-3 review-gate branch) denies AUTONOMOUS merge to an epic PR that CLAIMS a
// step done without backing it — a checked [x] box with no evidence and no matching
// change — routing it to merge_handoff (a human) instead of the merge queue.
//
// This exercises the whole seam end-to-end over the in-memory fakeGitHub + a
// scripted mirror history (no creds, no network): epic registration ->
// store.EpicForHeadSHA SHA-tip detection -> project.Sender's ActionEnqueueMerge
// re-verify -> epicDenyReason -> RouteSelfMergeToHandoff. The negative control
// proves the SAME machinery merges an all-green epic autonomously, so the handoff
// is caused by the unbacked claim, not by the epic path itself.
package acceptance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/config"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/project"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// epicGateHistory is a scripted HistoryWriter: it serves the epic's branch tip
// (so SHA-tip detection matches the PR head to the epic), the epic contract file
// AS OF the head, and the actual base..head diff.
type epicGateHistory struct {
	refTips map[string]string            // ref -> tip SHA
	files   map[string]map[string]string // ref -> path -> content
	diffOut string
}

func (h *epicGateHistory) CommitHistory(branch, message string, files []gitops.HistoryFile) (string, bool, error) {
	return "", false, nil
}
func (h *epicGateHistory) HeadSHA(ref string) (string, error) {
	if v, ok := h.refTips[ref]; ok {
		return v, nil
	}
	return "", nil
}
func (h *epicGateHistory) FetchBranch(branch string) error { return nil }
func (h *epicGateHistory) DiffBetween(base, head string) (string, error) {
	return h.diffOut, nil
}
func (h *epicGateHistory) ReadFileAtRef(ref, path string) (string, bool, error) {
	byPath, ok := h.files[ref]
	if !ok {
		return "", false, nil
	}
	c, ok := byPath[path]
	return c, ok, nil
}

// epicSpecFile renders a minimal, parseable epic spec with the given ## Status.
func epicSpecFile(state, blockers, checklist string) string {
	return "---\ntitle: Foo\nscope:\n  - app/foo/**\n---\n\n" +
		"## Goal\n\nDo the thing.\n\n" +
		"## Steps\n\n" +
		"1. First step\nValidate: go test ./app/foo/...\n\n" +
		"2. Second step\nValidate: go test ./app/foo/bar/...\n\n" +
		"## Status\n\nUpdated: 2026-07-03T12:00:00Z\nCurrent: step 2/2\nState: " + state + "\n\n" +
		checklist + "\n\nBlockers: " + blockers + "\n"
}

func epicDiffAdding(path, line string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"--- a/" + path + "\n+++ b/" + path + "\n@@ -0,0 +1 @@\n+" + line + "\n"
}

// newEpicGateEnv builds a store + project.Sender wired over a fakeGitHub with
// self-merge ENABLED (Branch B), so an all-green PR would merge autonomously and
// only the epic gate can force a handoff.
func newEpicGateEnv(t *testing.T) (*store.Store, *gh.Fake, *project.Sender, *clock.Fake) {
	t.Helper()
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 500 * time.Millisecond,
		LeaseTTLS: 300, HeartbeatIntervalS: 30,
		Policy:        job.Policy{AllowSelfMerge: true},
		ContentPolicy: config.Default().ContentPolicy(),
	}, "epic-gate")
	fake := gh.NewFake()
	sender := project.New(st, fake, clk, srv.Broker())
	return st, fake, sender, clk
}

// seedMergingEpicJob puts an approved, self-merge-authorized, CI-green job into
// `merging` with a pending ActionEnqueueMerge outbox row — the exact state the
// sender's autonomous-merge re-verify (and the epic gate within it) acts on.
func seedMergingEpicJob(t *testing.T, st *store.Store, id, repo, headSHA string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "base-sha", Repo: repo, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	v := job.MintVerdict(job.VerdictApproved, job.DispositionSelfMerge, headSHA, "base-sha")
	vb, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='merging', base_sha='base-sha', head_sha=?, verdict=?, issue_number=42 WHERE id=?`,
		headSHA, string(vb), id); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueOutbox(ctx, store.OutboxRow{
		JobID: id, Action: store.ActionEnqueueMerge, HeadSHA: headSHA, Payload: `{"pr_number":42}`,
	}); err != nil {
		t.Fatal(err)
	}
}

func registerEpicRun(t *testing.T, st *store.Store, repo, id, branch, filePath string) {
	t.Helper()
	if err := st.AddEpicRun(context.Background(), store.EpicRun{
		ID: id, Repo: repo, FilePath: filePath, Title: "Foo",
		Scope: []string{"app/foo/**"}, Branch: branch, TmuxName: "epic-" + id,
	}, 1, time.Unix(500, 0)); err != nil {
		t.Fatalf("register epic: %v", err)
	}
}

// setEpicLiveGreenPR mirrors the project package's setLiveGreenPR (a green,
// required-checks-satisfied PR the sender re-verifies before an autonomous merge).
func setEpicLiveGreenPR(fake *gh.Fake, number int, base, head string) {
	fake.SetPR(gh.PullRequest{
		Number: number, HeadRefOid: head, BaseRefOid: base,
		CIRollup: gh.CISuccess, PassedChecks: []string{"ci"},
	})
	fake.SetBranchProtection("main", gh.Protection{RequiredChecks: []string{"ci"}})
}

func merged(fake *gh.Fake) bool {
	for _, c := range fake.Calls() {
		if c == "EnqueueMergeQueue(42)" {
			return true
		}
	}
	return false
}

const epicP4SpecPath = "epics/2026-07-03-foo.md"

// epicP4FilesBaseHead serves the spec at BOTH the launch-pinned base ("base-sha", what
// seedMergingEpicJob authorizes) and the PR head. Review M1: the gate reads the Goal/Steps
// contract from the PINNED base and only the claimed ## Status from head. The base carries
// the standard (unedited) ## Steps; its own status is irrelevant.
func epicP4FilesBaseHead(head string) map[string]map[string]string {
	return map[string]map[string]string{
		"base-sha":      {epicP4SpecPath: epicSpecFile("building", "none", "")},
		"epic-head-sha": {epicP4SpecPath: head},
	}
}

// TestEpicGateP4_ClaimedStepWithoutEvidenceRoutesToHandoff is the Phase-4 acceptance:
// an epic PR whose ## Status checks Step 2 [x] but backs it with NO EVIDENCE is DENIED
// autonomous merge and routed to merge_handoff — even though State is done, blockers are
// empty, and the diff is in scope. The deterministic gate fires on the empty-evidence
// prong (a checked box with no substantive evidence string); per-step diff-correspondence
// ("this checked step has a matching change in the diff") is NOT enforced here — that is
// the cross-family reviewer's job (brief §15.8) and Phase 8's claim_exceeds_commits.
func TestEpicGateP4_ClaimedStepWithoutEvidenceRoutesToHandoff(t *testing.T) {
	st, fake, sender, _ := newEpicGateEnv(t)
	ctx := context.Background()

	registerEpicRun(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	// Step 1 is genuinely evidenced; Step 2 is CHECKED but carries no evidence.
	checklist := "- [x] Step 1 — first step (evidence: go test passed)\n" +
		"- [x] Step 2 — second step"
	hist := &epicGateHistory{
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		diffOut: epicDiffAdding("app/foo/a.go", "// step 1 work") +
			epicDiffAdding("epics/2026-07-03-foo.md", "- [x] Step 2 — second step"),
		files: epicP4FilesBaseHead(epicSpecFile("done", "none", checklist)),
	}
	sender.WithHistory(hist, "main")
	seedMergingEpicJob(t, st, "j", "russ", "epic-head-sha")
	setEpicLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if merged(fake) {
		t.Fatal("an epic PR claiming a step done with no evidence must NOT self-merge")
	}
	j, _ := st.GetJob(ctx, "j")
	if j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff (unevidenced step claim)", j.State)
	}
}

// TestEpicGateP4_HeadRewritesScopeAndStepsDenied is the review-M1 pin: an epic PR that
// EDITS ITS OWN CONTRACT at head to self-certify — widening scope: to '**' so an
// out-of-bounds file "passes", and dropping a ## Steps entry so a smaller checklist looks
// complete — is DENIED. The gate judges scope against the launch-pinned e.Scope (not the
// head frontmatter) and Steps against the pinned base (not head), so both edits are
// caught: the out-of-scope file trips CheckScope and the shrunk head Steps trip the
// spec-immutability breach. Without the M1 fix this PR would have self-merged.
func TestEpicGateP4_HeadRewritesScopeAndStepsDenied(t *testing.T) {
	st, fake, sender, _ := newEpicGateEnv(t)
	ctx := context.Background()

	registerEpicRun(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	// the lying HEAD contract: scope widened to '**', and only ONE step (renumbered) that
	// the status marks done — so a naive head-trusting gate would pass both scope + evidence.
	headLie := "---\ntitle: Foo\nscope:\n  - '**'\n---\n\n" +
		"## Goal\n\nDo the thing.\n\n" +
		"## Steps\n\n1. Only step\nValidate: go test ./...\n\n" +
		"## Status\n\nUpdated: 2026-07-03T12:00:00Z\nCurrent: step 1/1\nState: done\n\n" +
		"- [x] Step 1 — only step (evidence: go test passed)\n\nBlockers: none\n"
	hist := &epicGateHistory{
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		// touches a file OUTSIDE the pinned app/foo/** scope — only the head's forged '**'
		// would admit it.
		diffOut: epicDiffAdding("internal/secret/backdoor.go", "// out of pinned scope") +
			epicDiffAdding("epics/2026-07-03-foo.md", "forged status"),
		files: epicP4FilesBaseHead(headLie),
	}
	sender.WithHistory(hist, "main")
	seedMergingEpicJob(t, st, "j", "russ", "epic-head-sha")
	setEpicLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if merged(fake) {
		t.Fatal("an epic PR that rewrote its own scope:/Steps at head must NOT self-merge")
	}
	if j, _ := st.GetJob(ctx, "j"); j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s, want merge_handoff (contract rewritten at head)", j.State)
	}
}

// TestEpicGateP4_AllGreenMergesAutonomously is the negative control: the SAME
// harness, with Step 2 properly evidenced, merges autonomously via the merge queue.
// Together the tests prove the handoffs above are caused by the specific violation,
// not by the epic path itself.
func TestEpicGateP4_AllGreenMergesAutonomously(t *testing.T) {
	st, fake, sender, _ := newEpicGateEnv(t)
	ctx := context.Background()

	registerEpicRun(t, st, "russ", "2026-07-03-foo", "epic/2026-07-03-foo", "epics/2026-07-03-foo.md")
	checklist := "- [x] Step 1 — first step (evidence: go test passed)\n" +
		"- [x] Step 2 — second step (evidence: go test bar passed)"
	hist := &epicGateHistory{
		refTips: map[string]string{"refs/heads/epic/2026-07-03-foo": "epic-head-sha"},
		diffOut: epicDiffAdding("app/foo/a.go", "// step 1 work") +
			epicDiffAdding("epics/2026-07-03-foo.md", "- [x] Step 2 done"),
		files: epicP4FilesBaseHead(epicSpecFile("done", "none", checklist)),
	}
	sender.WithHistory(hist, "main")
	seedMergingEpicJob(t, st, "j", "russ", "epic-head-sha")
	setEpicLiveGreenPR(fake, 42, "base-sha", "epic-head-sha")

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !merged(fake) {
		j, _ := st.GetJob(ctx, "j")
		t.Fatalf("an all-green epic PR must merge autonomously; calls=%v state=%s", fake.Calls(), j.State)
	}
}
