// F7 acceptance: board lifecycle — backlog, the needs_design user-agent loop, the
// yellow `flowbee` umbrella label, and direct-to-GitHub issues adopted
// mirrored-quiescent (opt-in via flowbee:adopt).
//
// Proven end-to-end over the real HTTP worker/operator surface against the
// in-memory fakeGitHub (no real creds, no network). Workers hold no GitHub creds;
// Flowbee renders every label OUT through project-OUT (R4).
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - a `backlog` item is tracked but NOT scheduled until promoted (the atomic
//     claim refuses it; a promoted item becomes leasable);
//   - a needs_design item appears on GET /v1/needs-input and resumes to spec_review
//     when an answer is POSTed to /v1/jobs/{job}/design (the user-agent loop);
//   - the yellow `flowbee` umbrella label (+ a per-stage label) is rendered on
//     every actively-tracked issue via project-OUT (asserted on fakeGitHub);
//   - a direct-to-GitHub issue is mirrored-quiescent by default and opts in via a
//     flowbee:adopt label into a single-issue flow entering at issue-review.
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// ── TestF7_BacklogNotScheduledUntilPromoted: the backlog state (§D). ──
func TestF7_BacklogNotScheduledUntilPromoted(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()
	url := e.private.URL

	// a tracked-but-not-scheduled, ready-to-build backlog item.
	id := "f7-backlog-build"
	if _, err := e.st.SeedBacklog(ctx, store.SeedBacklogParams{
		ID: id, ChatRef: "chat-bk", Priority: 1, NeedsFullSpec: false,
		TaskText: "wire the thing", Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}

	// it appears on GET /v1/backlog (the Backlog lane the user-agent reads).
	items := getBacklog(t, url)
	if len(items) != 1 || items[0].JobID != id || items[0].NeedsFullSpec {
		t.Fatalf("backlog lane must surface the item, got %+v", items)
	}

	// a worker CANNOT lease it (it is not scheduled): the long-poll finds nothing.
	w := registerCaps(t, ctx, url, "bk-bob", "codex", []string{"role:eng_worker", "model_family:codex"})
	if g, ok, _ := w.Lease(ctx, "bk-bob", "codex", ""); ok {
		t.Fatalf("a backlog item must NOT be leasable, got %s", g.JobID)
	}
	if j, _ := e.st.GetJob(ctx, id); j.State != job.StateBacklog {
		t.Fatalf("a backlog item must stay backlog, got %s", j.State)
	}

	// PROMOTE it via POST /v1/jobs/{job}/promote (the deliberate operator edge).
	st := postPromote(t, url, id)
	if st != string(job.StateReady) {
		t.Fatalf("a ready-to-build item must promote to ready, got %s", st)
	}
	if got := getBacklog(t, url); len(got) != 0 {
		t.Fatalf("a promoted item must leave the backlog lane, got %+v", got)
	}

	// now the worker CAN lease it (it is genuinely scheduled).
	g, ok, err := w.Lease(ctx, "bk-bob", "codex", "")
	if err != nil || !ok || g.JobID != id {
		t.Fatalf("a promoted ready item must be leasable, ok=%v err=%v job=%s", ok, err, g.JobID)
	}
}

// ── TestF7_UserAgentDesignLoopOverHTTP: post an answer -> resume (§D). ──
// A needs_design item appears on GET /v1/needs-input; the user-agent posts the
// answer to POST /v1/jobs/{job}/design and Flowbee resumes it to spec_review,
// where the reviewer can now sign it off.
func TestF7_UserAgentDesignLoopOverHTTP(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()
	url := e.private.URL

	specJob := "f7-design"
	if _, err := e.st.SeedSpecJob(ctx, store.SeedSpecParams{
		ID: specJob, ChatRef: "chat-d7", AuthorLens: "product_speccer", Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	author := registerCaps(t, ctx, url, "f7-auth", "opus", []string{"role:spec_author", "model_family:opus"})
	ag, _, _ := author.Lease(ctx, "f7-auth", "opus", string(job.RoleSpecAuthor))
	const body = "# Feature needing a product call\n\nShard by tenant or region?\n"
	h, _, _, err := author.SpecSubmit(ctx, specJob, ag.LeaseEpoch, body, 1)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// the issue-reviewer flags a design fork.
	reviewer := registerCaps(t, ctx, url, "f7-em", "codex", []string{"role:spec_reviewer", "model_family:codex"})
	rg, _, _ := reviewer.LeaseWithLens(ctx, "f7-em", "codex", string(job.RoleSpecReviewer), "engineering_manager")
	resp, code, err := reviewer.SpecReviewNeedsDesign(ctx, specJob, rg.LeaseEpoch, "f7-nd", h)
	if err != nil || code != http.StatusOK || !resp.NeedsDesign {
		t.Fatalf("design fork code=%d resp=%+v err=%v", code, resp, err)
	}

	// the user-agent loop: read /v1/needs-input, see the item.
	items := getNeedsInput(t, url)
	if len(items) != 1 || items[0].JobID != specJob {
		t.Fatalf("the design fork must surface on /v1/needs-input, got %+v", items)
	}

	// POST the answer -> Flowbee resumes the job to spec_review.
	out := postDesignAnswer(t, url, specJob, "")
	if out["state"] != string(job.StateSpecReview) {
		t.Fatalf("posting an answer must resume spec_review, got %v", out)
	}
	if got := getNeedsInput(t, url); len(got) != 0 {
		t.Fatalf("a resumed item must leave /v1/needs-input, got %+v", got)
	}

	// the reviewer can now sign off the resumed spec (the loop is closed).
	rg2, _, _ := reviewer.LeaseWithLens(ctx, "f7-em", "codex", string(job.RoleSpecReviewer), "engineering_manager")
	good, _, _ := reviewer.SpecReview(ctx, specJob, rg2.LeaseEpoch, "f7-ok", "signed_off", h, true, true)
	if !good.Minted {
		t.Fatalf("after the answer, a sign-off must mint, got %+v", good)
	}
}

// ── TestF7_UmbrellaLabelRendered: the yellow `flowbee` label (§D). ──
// An actively-tracked issue carries the yellow `flowbee` umbrella label + a per-
// stage label, rendered OUT via project-OUT to fakeGitHub. A quiescent adopted
// issue is NOT labelled (never reasserted over human-owned work).
func TestF7_UmbrellaLabelRendered(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()
	now := e.clk.Now()

	// a tracked spec job bound to GitHub issue #310 (promoted from backlog).
	id := "f7-tracked"
	if _, err := e.st.SeedBacklog(ctx, store.SeedBacklogParams{
		ID: id, ChatRef: "chat-t", IssueNumber: 310, NeedsFullSpec: true, Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.st.PromoteBacklog(ctx, id, now); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// enqueue the umbrella + stage label render and drain it OUT (Flowbee is the sole
	// GitHub writer; the worker never touches labels).
	enq, err := e.st.EnqueueTrackingLabels(ctx, id, now)
	if err != nil || !enq {
		t.Fatalf("EnqueueTrackingLabels enq=%v err=%v", enq, err)
	}
	if e.drain(t, ctx) != 1 {
		t.Fatalf("exactly one project-OUT label render expected")
	}
	labels := e.fake.Labels(310)
	if !contains(labels, store.UmbrellaLabel) {
		t.Fatalf("the yellow `flowbee` umbrella label must be rendered on #310, got %v", labels)
	}
	if !contains(labels, store.StageLabel(job.StateSpecAuthoring)) {
		t.Fatalf("a per-stage label must accompany the umbrella, got %v", labels)
	}

	// the render is audited exactly once (keyed per stage) — no duplicate.
	audit, _ := e.st.AuditLog(ctx, id)
	labelActions := 0
	for _, a := range audit {
		if a.Action == store.ActionSetLabels {
			labelActions++
		}
	}
	if labelActions != 1 {
		t.Fatalf("the label render must be audited once, got %d", labelActions)
	}

	// a quiescent adopted issue is NEVER labelled (the §8.2.3 / I-16 suppression).
	e.fake.SetIssue(gh.Issue{Number: 320, UpdatedAt: now, Body: "drive-by"})
	snap, _ := e.fake.BoardSweep(ctx)
	if _, err := e.st.AdoptSweep(ctx, snap, time.Unix(1, 0), now); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	enq, _ = e.st.EnqueueTrackingLabels(ctx, "adopt-issue-320", now)
	if enq {
		t.Fatal("a quiescent adopted issue must NOT be labelled")
	}
}

// ── TestF7_DirectIssueAdoptViaLabel: direct-to-GitHub issues (§D). ──
// A plain open issue is mirrored-quiescent (never scheduled); an issue carrying a
// flowbee:adopt label opts in to a single-issue flow entering at issue-review.
func TestF7_DirectIssueAdoptViaLabel(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()
	now := e.clk.Now()

	e.fake.SetIssue(gh.Issue{Number: 400, UpdatedAt: now, Title: "noise", Body: "ignore"})
	e.fake.SetIssue(gh.Issue{
		Number: 401, UpdatedAt: now, Labels: []string{"flowbee:adopt"},
		Title: "add caching", Body: "Add a cache.\n\n## Done When\ncache hits 90%",
	})
	snap, _ := e.fake.BoardSweep(ctx)
	adopted, err := e.st.AdoptSweep(ctx, snap, time.Unix(1, 0), now)
	if err != nil || len(adopted) != 2 {
		t.Fatalf("both issues must import, got %v err=%v", adopted, err)
	}

	// #400 is quiescent (mirrored, never scheduled).
	q, _ := e.st.GetJob(ctx, "adopt-issue-400")
	if q.State != job.StateQuiescent {
		t.Fatalf("a non-adopt issue must be quiescent, got %s", q.State)
	}

	// #401 entered issue-review (spec_review) with its parsed body as task/spec.
	r, _ := e.st.GetJob(ctx, "adopt-issue-401")
	if r.State != job.StateSpecReview || r.IssueNum != 401 || r.AcceptanceCriteria != "cache hits 90%" {
		t.Fatalf("a flowbee:adopt issue must enter issue-review with its body, got %+v", r)
	}

	// the opted-in issue is leasable by a spec_reviewer (the single-issue flow runs).
	url := e.private.URL
	reviewer := registerCaps(t, ctx, url, "f7-irev", "codex", []string{"role:spec_reviewer", "model_family:codex"})
	g, ok, err := reviewer.LeaseWithLens(ctx, "f7-irev", "codex", string(job.RoleSpecReviewer), "engineering_manager")
	if err != nil || !ok || g.JobID != "adopt-issue-401" {
		t.Fatalf("the opted-in issue must be the leasable issue-review, ok=%v err=%v job=%s", ok, err, g.JobID)
	}

	// the manual opt-in edge promotes the quiescent #400 into issue-review too.
	postAdopt(t, url, "adopt-issue-400")
	q, _ = e.st.GetJob(ctx, "adopt-issue-400")
	if q.State != job.StateSpecReview {
		t.Fatalf("a manual opt-in of an issue must enter issue-review, got %s", q.State)
	}
}

// ── HTTP helpers (the operator / user-agent surface) ──

func getBacklog(t *testing.T, url string) []store.BacklogItem {
	t.Helper()
	resp, err := http.Get(url + "/v1/backlog")
	if err != nil {
		t.Fatalf("backlog get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backlog status %d", resp.StatusCode)
	}
	var out []store.BacklogItem
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("backlog decode: %v", err)
	}
	return out
}

func postPromote(t *testing.T, url, jobID string) string {
	t.Helper()
	out := postJSON(t, url+"/v1/jobs/"+jobID+"/promote", nil)
	s, _ := out["state"].(string)
	return s
}

func postDesignAnswer(t *testing.T, url, jobID, amended string) map[string]any {
	t.Helper()
	var body any
	if amended != "" {
		body = map[string]any{"amended_spec_markdown": amended, "amended_version": 2}
	}
	return postJSON(t, url+"/v1/jobs/"+jobID+"/design", body)
}

func postAdopt(t *testing.T, url, jobID string) {
	t.Helper()
	postJSON(t, url+"/v1/jobs/"+jobID+"/adopt", nil)
}

func postJSON(t *testing.T, target string, body any) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	resp, err := http.Post(target, "application/json", &buf)
	if err != nil {
		t.Fatalf("post %s: %v", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post %s status %d", target, resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}
