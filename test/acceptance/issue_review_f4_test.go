// F4 acceptance: amend-in-place issue-review + needs_design + epic-level review.
//
// The flow-pass "amend vs bounce" decision, proven end-to-end against the in-memory
// fakeGitHub over the real HTTP worker surface (no real creds, no network). It fixes
// the M7 spec flow (which built bounce-to-author) so issue-review AMENDS the spec in
// place and commits it, and adds the design-fork + epic-barrier behaviors.
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - issue-review AMENDS a sub-standard spec and proceeds (commits the amended spec,
//     mints a sign-off bound to the AMENDED bytes, materializes the issue) with NO
//     bounce to the user/spec_author;
//   - a design fork -> needs_design (surfaced on GET /v1/needs-input), resumable;
//   - issue-review runs ONCE at the EPIC level (a barrier over ALL the epic's issues)
//     BEFORE any issue fans out;
//   - build-review still BOUNCES to the build agent (unchanged).
package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/spec"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// ── TestF4_IssueReviewAmendsInPlaceNoBounce: the core flow-pass fix (§B). ──
// A sub-standard spec is reviewed by the engineering_manager (issue-reviewer). Rather
// than bouncing it to the author, the reviewer AMENDS it in place: Flowbee commits
// the amended bytes, the gate mints a sign-off bound to the AMENDED hash, the issue
// materializes. The job NEVER returns to spec_authoring (no author bounce).
func TestF4_IssueReviewAmendsInPlaceNoBounce(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()
	url := e.private.URL

	specJob := "spec-amend"
	if _, err := e.st.SeedSpecJob(ctx, store.SeedSpecParams{
		ID: specJob, ChatRef: "chat-amend", AuthorLens: "product_speccer", Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed spec job: %v", err)
	}

	// the author drafts a SUB-STANDARD spec (it fails the reviewer's sub-checks).
	author := registerCaps(t, ctx, url, "author-amy", "opus", []string{"role:spec_author", "model_family:opus"})
	ag, ok, err := author.Lease(ctx, "author-amy", "opus", string(job.RoleSpecAuthor))
	if err != nil || !ok || ag.JobID != specJob {
		t.Fatalf("author lease ok=%v err=%v job=%s", ok, err, ag.JobID)
	}
	const substandard = "# Feature\n\nvague one-liner, no acceptance criteria.\n"
	h1, _, st, err := author.SpecSubmit(ctx, specJob, ag.LeaseEpoch, substandard, 1)
	if err != nil || st != http.StatusOK {
		t.Fatalf("spec submit st=%d err=%v", st, err)
	}

	// the issue-reviewer (engineering_manager lens, distinct from the author) judges
	// the spec sub-standard and AMENDS it in place rather than bouncing to the author.
	reviewer := registerCaps(t, ctx, url, "em-erin", "codex", []string{"role:spec_reviewer", "model_family:codex"})
	rg, ok, err := reviewer.LeaseWithLens(ctx, "em-erin", "codex", string(job.RoleSpecReviewer), "engineering_manager")
	if err != nil || !ok || rg.JobID != specJob {
		t.Fatalf("reviewer lease ok=%v err=%v job=%s", ok, err, rg.JobID)
	}
	if rg.SpecContentHash != h1 {
		t.Fatalf("the lease must carry the reviewed hash %q, got %q", h1, rg.SpecContentHash)
	}
	const amended = "# Feature\n\n## Goal\nBuild X.\n\n## Acceptance\n- [ ] X works\n- [ ] tests green\n"
	amendedHash := spec.ContentHash([]byte(amended))
	resp, code, err := reviewer.SpecReviewAmend(ctx, specJob, rg.LeaseEpoch, "am-1", h1, amended, 2)
	if err != nil || code != http.StatusOK {
		t.Fatalf("spec amend code=%d err=%v", code, err)
	}
	if !resp.Amended || !resp.Minted {
		t.Fatalf("the issue-review must AMEND in place + mint, got %+v", resp)
	}
	if resp.JobState != string(job.StateDone) {
		t.Fatalf("an amended spec must reach done, got %s", resp.JobState)
	}

	// the spec advanced IN PLACE to the amended bytes; the sign-off binds to them.
	j, _ := e.st.GetJob(ctx, specJob)
	if j.SpecContentHash != amendedHash {
		t.Fatalf("the spec must advance in place to the amended hash, got %q want %q", j.SpecContentHash, amendedHash)
	}
	if j.SpecSignoff == nil || !j.SpecSignoff.Verify(amendedHash) {
		t.Fatalf("the sign-off must be tamper-evident + bound to the AMENDED bytes: %+v", j.SpecSignoff)
	}
	if j.SpecSignoff.Verify(h1) {
		t.Fatalf("the sign-off must NOT bind to the original sub-standard bytes")
	}

	// CRITICAL: the job NEVER bounced to the author. No spec_bounced/spec_superseded
	// event exists in the ledger; the journey is amend -> done, and bounces stayed 0.
	evs, _ := e.st.LoadEvents(ctx, specJob)
	for _, ev := range evs {
		// a bounce-to-author is a spec_review -> spec_authoring STATE MOVE (a
		// spec_bounced/spec_superseded event). The author's lease_claimed event has
		// ToState==spec_authoring too but FromState==spec_authoring (no move) — exclude it.
		if ev.Kind == ledger.KindSpecBounced || ev.Kind == ledger.KindSpecSuperseded {
			t.Fatalf("issue-review must NEVER bounce to the author, saw %s", ev.Kind)
		}
		if ev.FromState == job.StateSpecReview && ev.ToState == job.StateSpecAuthoring {
			t.Fatalf("issue-review must NEVER move spec_review -> spec_authoring, saw %s", ev.Kind)
		}
	}
	if j.Bounces != 0 {
		t.Fatalf("an amend is not a bounce: bounces=%d want 0", j.Bounces)
	}
	sawAmend := false
	for _, ev := range evs {
		if ev.Kind == ledger.KindSpecAmended {
			sawAmend = true
		}
	}
	if !sawAmend {
		t.Fatalf("the ledger must record a spec_amended event")
	}

	// the amended spec MATERIALIZES a real issue (project-OUT), keyed once.
	if e.drain(t, ctx) != 1 {
		t.Fatalf("exactly one project-OUT action (issues.create) expected")
	}
	j, _ = e.st.GetJob(ctx, specJob)
	if j.IssueNum == 0 || len(e.fake.Issues()) != 1 {
		t.Fatalf("the amended spec must materialize one real issue, issue=%d ghIssues=%d", j.IssueNum, len(e.fake.Issues()))
	}

	// determinism: Fold(events) reproduces the amended projection byte-for-byte.
	folded, _ := ledger.Fold(evs2(t, e, specJob))
	if folded.State != job.StateDone || folded.SpecContentHash != amendedHash {
		t.Fatalf("Fold != projection after amend: %s %s", folded.State, folded.SpecContentHash)
	}
	if folded.SpecSignoff == nil || folded.SpecSignoff.IntegrityHash != j.SpecSignoff.IntegrityHash {
		t.Fatalf("Fold != projection for the amended sign-off")
	}
}

// ── TestF4_DesignForkNeedsDesign: a design fork flags needs_design (§D). ──
// When the issue-reviewer determines the spec needs human DESIGN input (not a thing
// it can fix by amending), it flags needs_design. The job parks (no lease, not
// scheduled), surfaces on GET /v1/needs-input, and resumes to spec_review on resolve.
func TestF4_DesignForkNeedsDesign(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()
	url := e.private.URL

	specJob := "spec-design"
	if _, err := e.st.SeedSpecJob(ctx, store.SeedSpecParams{
		ID: specJob, ChatRef: "chat-design", AuthorLens: "product_speccer", Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	author := registerCaps(t, ctx, url, "auth-d", "opus", []string{"role:spec_author", "model_family:opus"})
	ag, _, _ := author.Lease(ctx, "auth-d", "opus", string(job.RoleSpecAuthor))
	const body = "# Feature needing a product decision\n\nShould we shard by tenant or by region?\n"
	h, _, _, err := author.SpecSubmit(ctx, specJob, ag.LeaseEpoch, body, 1)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	reviewer := registerCaps(t, ctx, url, "em-d", "codex", []string{"role:spec_reviewer", "model_family:codex"})
	rg, _, _ := reviewer.LeaseWithLens(ctx, "em-d", "codex", string(job.RoleSpecReviewer), "engineering_manager")
	resp, code, err := reviewer.SpecReviewNeedsDesign(ctx, specJob, rg.LeaseEpoch, "nd-1", h)
	if err != nil || code != http.StatusOK {
		t.Fatalf("needs_design code=%d err=%v", code, err)
	}
	if !resp.NeedsDesign || resp.Minted {
		t.Fatalf("a design fork must flag needs_design without minting, got %+v", resp)
	}
	if resp.JobState != string(job.StateNeedsDesign) {
		t.Fatalf("a design fork must reach needs_design, got %s", resp.JobState)
	}

	// the job parks: it holds NO active lease (not scheduled).
	j, _ := e.st.GetJob(ctx, specJob)
	if j.State != job.StateNeedsDesign || job.HasActiveLease(j.State) {
		t.Fatalf("needs_design must be a parked (no-active-lease) state, got %s", j.State)
	}

	// the job SURFACES on GET /v1/needs-input (the user's board-check loop reads it).
	items := getNeedsInput(t, url)
	found := false
	for _, it := range items {
		if it.JobID == specJob {
			found = true
			if it.Reason != string(job.EscalationDesign) {
				t.Fatalf("needs-input must tag the design reason, got %q", it.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("the design-fork job must appear on /v1/needs-input, got %+v", items)
	}
	// no issue was materialized (a design fork is not a sign-off).
	if e.drain(t, ctx) != 0 || len(e.fake.Issues()) != 0 {
		t.Fatalf("a design fork must materialize no issue")
	}

	// the human resolves the design decision; the job re-arms to spec_review and the
	// reviewer can now sign it off (no longer on /v1/needs-input).
	if err := e.st.ResolveDesign(ctx, specJob, "", 0, e.clk.Now()); err != nil {
		t.Fatalf("resolve design: %v", err)
	}
	j, _ = e.st.GetJob(ctx, specJob)
	if j.State != job.StateSpecReview {
		t.Fatalf("resolve must re-arm spec_review, got %s", j.State)
	}
	if got := getNeedsInput(t, url); len(got) != 0 {
		t.Fatalf("a resolved job must leave /v1/needs-input, got %+v", got)
	}
	rg2, _, _ := reviewer.LeaseWithLens(ctx, "em-d", "codex", string(job.RoleSpecReviewer), "engineering_manager")
	good, _, _ := reviewer.SpecReview(ctx, specJob, rg2.LeaseEpoch, "ok", "signed_off", h, true, true)
	if !good.Minted {
		t.Fatalf("after resolve, a sign-off must mint, got %+v", good)
	}
}

// ── TestF4_EpicLevelReviewBarrierBeforeFanOut: the epic barrier (§B). ──
// Issue-review runs ONCE at the EPIC level — a barrier over ALL the epic's issues —
// before any issue fans out. The children sit in `backlog` (tracked, NOT scheduled);
// only after the epic-level review passes do they fan out into the per-issue flow.
func TestF4_EpicLevelReviewBarrierBeforeFanOut(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()
	url := e.private.URL

	epicID := "epic-1"
	children := []string{"epic-1-iss-a", "epic-1-iss-b", "epic-1-iss-c"}
	if err := e.st.SeedEpic(ctx, store.SeedEpicParams{
		EpicID: epicID, ChatRef: "chat-epic", AuthorLens: "product_speccer",
		IssueIDs: children, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed epic: %v", err)
	}

	// BEFORE the barrier: every child sits in backlog (tracked, NOT scheduled). A
	// spec_author MUST NOT be able to lease any child (the barrier holds the epic).
	for _, c := range children {
		j, _ := e.st.GetJob(ctx, c)
		if j.State != job.StateBacklog || j.EpicID != epicID {
			t.Fatalf("child %s must be in backlog under the epic, got state=%s epic=%s", c, j.State, j.EpicID)
		}
		if job.HasActiveLease(j.State) {
			t.Fatalf("a backlog child must not hold an active lease")
		}
	}
	author := registerCaps(t, ctx, url, "auth-e", "opus", []string{"role:spec_author", "model_family:opus"})
	if g, ok, _ := author.Lease(ctx, "auth-e", "opus", string(job.RoleSpecAuthor)); ok {
		t.Fatalf("no epic child may be leased before the epic-level review (got %s)", g.JobID)
	}
	// fanning out before the review is refused (the barrier must hold).
	if _, err := e.st.EpicFanOut(ctx, epicID, e.clk.Now()); err == nil {
		t.Fatalf("fan-out before the epic review must be refused")
	}

	// the ONE epic-level issue-review: a single reviewer judges the WHOLE epic (scope
	// · coverage · dep-graph · standards). Note: ONE review, not one per child.
	reviewer := registerCaps(t, ctx, url, "em-e", "codex", []string{"role:spec_reviewer", "model_family:codex"})
	rg, ok, err := reviewer.LeaseWithLens(ctx, "em-e", "codex", string(job.RoleSpecReviewer), "engineering_manager")
	if err != nil || !ok {
		t.Fatalf("epic reviewer lease ok=%v err=%v", ok, err)
	}
	if rg.JobID != epicID {
		t.Fatalf("the epic barrier is the leasable spec_review, got %s want %s", rg.JobID, epicID)
	}
	// the barrier's spec hash is its current (decomposition) content address; the
	// lease carries it for the reviewer to bind to.
	epicHash := rg.SpecContentHash
	resp, code, err := reviewer.SpecReview(ctx, epicID, rg.LeaseEpoch, "epic-rev", "signed_off", epicHash, true, true)
	if err != nil || code != http.StatusOK {
		t.Fatalf("epic review code=%d err=%v", code, err)
	}
	if !resp.Minted || resp.JobState != string(job.StateDone) {
		t.Fatalf("the epic review must pass the barrier (-> done), got %+v", resp)
	}
	if ok, _ := e.st.EpicReviewed(ctx, epicID); !ok {
		t.Fatalf("the epic must be marked reviewed after the barrier passes")
	}
	// the epic barrier records its own epic_reviewed event and materializes NO single
	// issue (its children are the issues).
	eevs, _ := e.st.LoadEvents(ctx, epicID)
	sawEpicReviewed := false
	for _, ev := range eevs {
		if ev.Kind == ledger.KindEpicReviewed {
			sawEpicReviewed = true
		}
	}
	if !sawEpicReviewed {
		t.Fatalf("the epic barrier must record an epic_reviewed event")
	}

	// the children are STILL in backlog until the explicit fan-out (the barrier gated
	// them; the review passing is the precondition, the fan-out is the release).
	for _, c := range children {
		if j, _ := e.st.GetJob(ctx, c); j.State != job.StateBacklog {
			t.Fatalf("child %s must remain backlog until fan-out, got %s", c, j.State)
		}
	}

	// NOW the issues fan out — all of them, exactly once, into the per-issue spec flow.
	released, err := e.st.EpicFanOut(ctx, epicID, e.clk.Now())
	if err != nil {
		t.Fatalf("fan out: %v", err)
	}
	if len(released) != len(children) {
		t.Fatalf("fan-out must release every child once, got %d want %d", len(released), len(children))
	}
	for _, c := range children {
		if j, _ := e.st.GetJob(ctx, c); j.State != job.StateSpecAuthoring {
			t.Fatalf("a fanned-out child must enter the spec flow (spec_authoring), got %s", j.State)
		}
	}
	// a fanned-out child is now leasable by a spec_author (the gate has lifted).
	g, ok, err := author.Lease(ctx, "auth-e", "opus", string(job.RoleSpecAuthor))
	if err != nil || !ok {
		t.Fatalf("a fanned-out child must be leasable, ok=%v err=%v", ok, err)
	}
	isChild := false
	for _, c := range children {
		if g.JobID == c {
			isChild = true
		}
	}
	if !isChild {
		t.Fatalf("the leased job must be an epic child, got %s", g.JobID)
	}
	// a re-fan-out is idempotent (no child is released twice).
	again, _ := e.st.EpicFanOut(ctx, epicID, e.clk.Now())
	if len(again) != 0 {
		t.Fatalf("re-fan-out must be a no-op, released %v", again)
	}
}

// ── TestF4_BuildReviewStillBouncesToBuildAgent: the asymmetry holds (§B). ──
// build-review BOUNCES to the build agent (never amends code) — the opposite of
// issue-review. A changes_requested build verdict returns the job to `ready` for a
// fresh eng_worker, with bounces incremented. (Proves F4 did not leak amend into
// the code-review gate.)
func TestF4_BuildReviewStillBouncesToBuildAgent(t *testing.T) {
	e := newM7Env(t, job.Policy{})
	ctx := context.Background()
	url := e.private.URL

	buildJob := "build-bounce"
	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: buildJob, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base-0",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: e.clk.Now(),
	}); err != nil {
		t.Fatalf("seed build: %v", err)
	}
	builder := registerCaps(t, ctx, url, "bob", "codex", []string{"role:eng_worker", "model_family:codex"})
	bg, ok, err := builder.Lease(ctx, "bob", "codex", "")
	if err != nil || !ok {
		t.Fatalf("builder lease ok=%v err=%v", ok, err)
	}
	if _, _, err := builder.Result(ctx, buildJob, bg.LeaseEpoch, "b-1",
		map[string]any{"kind": "patch", "base_sha": "base-0"}); err != nil {
		t.Fatalf("builder result: %v", err)
	}

	// a review is only offered once CI is reconciled green (ReviewPendingCandidates).
	if err := e.st.UpsertDomainBFacts(ctx, buildJob, job.DomainBFacts{
		PRExists: true, PRNumber: 7, HeadSHA: "head-0", BaseSHA: "base-0", CIGreen: true,
	}); err != nil {
		t.Fatalf("seed green CI facts: %v", err)
	}

	reviewer := registerCaps(t, ctx, url, "rev", "opus", []string{"role:code_reviewer", "model_family:opus"})
	rg, ok, err := reviewer.Lease(ctx, "rev", "opus", string(job.RoleCodeReviewer))
	if err != nil || !ok || rg.JobID != buildJob {
		t.Fatalf("reviewer lease ok=%v err=%v", ok, err)
	}
	rv, code, err := reviewer.Review(ctx, buildJob, rg.LeaseEpoch, "rv-1", "changes_requested", "", "")
	if err != nil || code != http.StatusOK {
		t.Fatalf("review code=%d err=%v", code, err)
	}
	if rv.Minted {
		t.Fatalf("a changes_requested build review must NOT mint")
	}
	// the build BOUNCES to the build agent: back to `ready` as an eng_worker, bounce++.
	j, _ := e.st.GetJob(ctx, buildJob)
	if j.State != job.StateReady {
		t.Fatalf("build-review must BOUNCE to ready (the build agent re-builds), got %s", j.State)
	}
	if j.Role != job.RoleEngWorker {
		t.Fatalf("a bounced build must re-arm as eng_worker, got %s", j.Role)
	}
	if j.Bounces != 1 {
		t.Fatalf("a build bounce must increment bounces, got %d", j.Bounces)
	}
}

// evs2 reloads a job's events (a tiny helper to keep the Fold assertion readable).
func evs2(t *testing.T, e *m7Env, jobID string) []ledger.Event {
	t.Helper()
	evs, err := e.st.LoadEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	return evs
}

// getNeedsInput reads GET /v1/needs-input (the F4 design-fork surface).
func getNeedsInput(t *testing.T, url string) []store.NeedsInputItem {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/v1/needs-input", nil)
	if err != nil {
		t.Fatalf("needs-input req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("needs-input do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("needs-input status %d", resp.StatusCode)
	}
	var out []store.NeedsInputItem
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("needs-input decode: %v", err)
	}
	return out
}
