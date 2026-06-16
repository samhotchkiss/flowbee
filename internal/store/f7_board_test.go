package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/project"
	"github.com/samhotchkiss/flowbee/internal/spec"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// fixedClock is a deterministic clock for the project-OUT sender (Flowbee is the
// sole clock; the core is fed time as a value).
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// claimAttempt runs the real §6.3.1 atomic claim against a job, returning the
// error so a test can assert a non-`ready` job is structurally un-leasable.
func claimAttempt(ctx context.Context, t *testing.T, st *store.Store, jobID string, now time.Time) error {
	t.Helper()
	_, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: jobID, LeaseID: "lease-" + jobID, Identity: "w1", ModelFamily: "fam_a",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker", "role:spec_author"},
		TTL: time.Minute, Now: now,
	})
	return err
}

// TestF7BacklogNotScheduledUntilPromoted proves the F7 `backlog` state: a backlog
// item is tracked + visible but structurally un-leasable (the atomic claim refuses
// it) and only becomes schedulable when deliberately promoted. A needs-full-spec
// item promotes into the spec flow; a ready-to-build item promotes to `ready`.
func TestF7BacklogNotScheduledUntilPromoted(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(5000, 0)

	// a tracked-but-not-scheduled backlog item that needs a full spec first.
	spec1 := "bk-needs-spec"
	if _, err := st.SeedBacklog(ctx, store.SeedBacklogParams{
		ID: spec1, ChatRef: "c", IssueNumber: 41, Priority: 5,
		NeedsFullSpec: true, TaskText: "design the widget", Now: now,
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	// a ready-to-build backlog item (no full spec needed).
	build1 := "bk-ready"
	if _, err := st.SeedBacklog(ctx, store.SeedBacklogParams{
		ID: build1, ChatRef: "c", Priority: 3, NeedsFullSpec: false, Now: now,
	}); err != nil {
		t.Fatalf("seed backlog 2: %v", err)
	}

	// both are in backlog, surfaced on the Backlog lane, and NOT in the ready set.
	bk, err := st.Backlog(ctx)
	if err != nil || len(bk) != 2 {
		t.Fatalf("Backlog lane must show both items, got %+v err=%v", bk, err)
	}
	// ordered by priority DESC: spec1 (5) before build1 (3).
	if bk[0].JobID != spec1 || !bk[0].NeedsFullSpec || bk[0].IssueNumber != 41 {
		t.Fatalf("backlog[0] wrong: %+v", bk[0])
	}
	if bk[1].JobID != build1 || bk[1].NeedsFullSpec {
		t.Fatalf("backlog[1] wrong: %+v", bk[1])
	}
	cands, _ := st.ReadyCandidates(ctx)
	if len(cands) != 0 {
		t.Fatalf("a backlog item must NOT be a ready candidate, got %+v", cands)
	}

	// the atomic claim REFUSES a backlog job (it is not `ready`): not scheduled.
	if err := claimAttempt(ctx, t, st, build1, now); !errors.Is(err, lease.ErrLostRace) {
		t.Fatalf("claiming a backlog job must lose the race, got %v", err)
	}
	if j, _ := st.GetJob(ctx, build1); j.State != job.StateBacklog {
		t.Fatalf("a refused claim must leave the job in backlog, got %s", j.State)
	}

	// PROMOTE the needs-full-spec item -> spec_authoring (enters the spec flow).
	to, err := st.PromoteBacklog(ctx, spec1, now)
	if err != nil || to != job.StateSpecAuthoring {
		t.Fatalf("promote needs-full-spec -> spec_authoring, got %s err=%v", to, err)
	}
	if j, _ := st.GetJob(ctx, spec1); j.State != job.StateSpecAuthoring {
		t.Fatalf("promoted spec item state=%s want spec_authoring", j.State)
	}

	// PROMOTE the ready-to-build item -> ready (now schedulable).
	to, err = st.PromoteBacklog(ctx, build1, now)
	if err != nil || to != job.StateReady {
		t.Fatalf("promote ready item -> ready, got %s err=%v", to, err)
	}
	cands, _ = st.ReadyCandidates(ctx)
	if len(cands) != 1 || cands[0].JobID != build1 {
		t.Fatalf("a promoted ready item must be a ready candidate, got %+v", cands)
	}
	// now the claim SUCCEEDS (it is genuinely scheduled).
	if err := claimAttempt(ctx, t, st, build1, now); err != nil {
		t.Fatalf("a promoted ready item must be leasable, got %v", err)
	}

	// promoting a non-backlog job is refused (the edge must hold).
	if _, err := st.PromoteBacklog(ctx, build1, now); err == nil {
		t.Fatal("promoting a non-backlog job must be refused")
	}

	// determinism: Fold(events) reproduces the promoted projection.
	evs, _ := st.LoadEvents(ctx, spec1)
	folded, _ := ledger.Fold(evs)
	if folded.State != job.StateSpecAuthoring {
		t.Fatalf("Fold != projection after promote: %s", folded.State)
	}
}

// TestF7UserAgentDesignLoop proves the F7 user-agent board-check loop end-to-end
// at the store layer: a needs_design item surfaces on the needs-input read-model
// and resumes (needs_design -> spec_review) when an answer is posted. This is the
// store half of "post an answer -> resume needs_design -> issue_review".
func TestF7UserAgentDesignLoop(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(6000, 0)

	id := "dl-1"
	if _, err := st.SeedSpecJob(ctx, store.SeedSpecParams{
		ID: id, ChatRef: "c", AuthorLens: "product_speccer", Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	hash := spec.ContentHash([]byte("which database do we standardize on?"))
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='spec_review', stage='review', role='spec_reviewer',
		 spec_content_hash=?, reviewer_lens='engineering_manager' WHERE id=?`, hash, id); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// issue-review flags a design fork -> needs_design (the machine should not decide).
	resp, err := st.SpecReviewResult(ctx, store.SpecReviewResultParams{
		JobID: id, Epoch: 0, Claim: job.VerdictNeedsDesign, BindsTo: hash, Now: now,
	})
	if err != nil || !resp.NeedsDesign {
		t.Fatalf("design fork must park needs_design, got %+v err=%v", resp, err)
	}

	// the user-agent loop reads /v1/needs-input (this read-model) and sees the item.
	items, err := st.NeedsInput(ctx)
	if err != nil || len(items) != 1 || items[0].JobID != id {
		t.Fatalf("needs-input must surface the design fork, got %+v err=%v", items, err)
	}

	// the human answers via an edited spec; Flowbee commits it + resumes spec_review.
	answered := spec.ContentHash([]byte("standardize on SQLite (pure-Go)"))
	if err := st.ResolveDesign(ctx, id, answered, 2, now); err != nil {
		t.Fatalf("resolve design: %v", err)
	}
	j, _ := st.GetJob(ctx, id)
	if j.State != job.StateSpecReview || j.SpecContentHash != answered || j.EscalationReason != "" {
		t.Fatalf("resume must re-arm spec_review on the answered bytes, got %+v", j)
	}
	if items, _ := st.NeedsInput(ctx); len(items) != 0 {
		t.Fatalf("a resumed item must leave needs-input, got %+v", items)
	}
}

// TestF7UmbrellaLabelRendered proves the F7 yellow `flowbee` umbrella label is
// rendered OUT (via project-OUT, through the fake GitHub) on every actively-tracked
// issue, alongside the per-stage label — and that an adopted-quiescent issue is NOT
// labelled (never reasserted over human-owned work).
func TestF7UmbrellaLabelRendered(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(7000, 0)
	fake := gh.NewFake()
	sender := project.New(st, fake, fixedClock{now}, nil)

	// an actively-tracked spec job bound to GitHub issue #77 (promoted from backlog).
	id := "tracked-1"
	if _, err := st.SeedBacklog(ctx, store.SeedBacklogParams{
		ID: id, ChatRef: "c", IssueNumber: 77, NeedsFullSpec: true, Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.PromoteBacklog(ctx, id, now); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// enqueue + drain the umbrella + stage label render.
	enq, err := st.EnqueueTrackingLabels(ctx, id, now)
	if err != nil || !enq {
		t.Fatalf("EnqueueTrackingLabels must enqueue for a tracked issue, enq=%v err=%v", enq, err)
	}
	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	labels := fake.Labels(77)
	if !hasStr(labels, store.UmbrellaLabel) {
		t.Fatalf("the yellow `flowbee` umbrella label must be rendered on issue #77, got %v", labels)
	}
	if !hasStr(labels, store.StageLabel(job.StateSpecAuthoring)) {
		t.Fatalf("the per-stage label must be rendered alongside, got %v", labels)
	}

	// idempotent for the SAME stage: a re-enqueue is a no-op (no duplicate render).
	enq, err = st.EnqueueTrackingLabels(ctx, id, now)
	if err != nil || enq {
		t.Fatalf("re-enqueue for the same stage must be a no-op, enq=%v err=%v", enq, err)
	}

	// an adopted-QUIESCENT issue must NOT be labelled (never reasserted, §8.2.3).
	fake.SetIssue(gh.Issue{Number: 88, UpdatedAt: now, Body: "drive-by issue"})
	if _, err := st.AdoptSweep(ctx, mustSweep(ctx, t, fake), time.Unix(1, 0), now); err != nil {
		t.Fatalf("adopt sweep: %v", err)
	}
	enq, err = st.EnqueueTrackingLabels(ctx, "adopt-issue-88", now)
	if err != nil {
		t.Fatalf("tracking labels on quiescent: %v", err)
	}
	if enq {
		t.Fatal("a quiescent adopted issue must NOT be labelled")
	}
}

// TestF7DirectIssueAdoptOptIn proves the F7 direct-to-GitHub issue lifecycle: an
// open issue is imported mirrored-quiescent by default; a flowbee:adopt label opts
// it in to a standalone single-issue flow entering at issue-review (spec_review),
// with its body parsed into the task/spec the reviewer judges.
func TestF7DirectIssueAdoptOptIn(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(8000, 0)
	fake := gh.NewFake()

	// #100: a plain drive-by issue (no flowbee:adopt) -> mirrored-quiescent.
	fake.SetIssue(gh.Issue{Number: 100, UpdatedAt: now, Title: "spurious", Body: "ignore me"})
	// #101: an opted-in issue (flowbee:adopt) -> single-issue review.
	fake.SetIssue(gh.Issue{
		Number: 101, UpdatedAt: now, Labels: []string{"flowbee:adopt"},
		Title: "add retry", Body: "Add a retry.\n\n## Acceptance Criteria\nretries 3x",
	})

	adopted, err := st.AdoptSweep(ctx, mustSweep(ctx, t, fake), time.Unix(1, 0), now)
	if err != nil {
		t.Fatalf("adopt sweep: %v", err)
	}
	if len(adopted) != 2 {
		t.Fatalf("both issues must be imported, got %v", adopted)
	}

	// #100 stays quiescent (never scheduled, never rendered OUT).
	q, _ := st.GetJob(ctx, "adopt-issue-100")
	if q.State != job.StateQuiescent {
		t.Fatalf("a non-opted issue must be quiescent, got %s", q.State)
	}
	if quiescent, _ := st.IsQuiescent(ctx, "adopt-issue-100"); !quiescent {
		t.Fatal("issue #100 must be quiescent")
	}

	// #101 entered issue-review (spec_review) with its parsed body as task/spec.
	r, _ := st.GetJob(ctx, "adopt-issue-101")
	if r.State != job.StateSpecReview {
		t.Fatalf("a flowbee:adopt issue must enter issue-review (spec_review), got %s", r.State)
	}
	if r.IssueNum != 101 || r.AcceptanceCriteria != "retries 3x" {
		t.Fatalf("the issue body must seed the single-issue flow, got num=%d accept=%q", r.IssueNum, r.AcceptanceCriteria)
	}
	if quiescent, _ := st.IsQuiescent(ctx, "adopt-issue-101"); quiescent {
		t.Fatal("an opted-in issue must NOT be quiescent")
	}

	// the manual OptIn edge promotes the quiescent #100 into issue-review too.
	if err := st.OptIn(ctx, "adopt-issue-100", now); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	q, _ = st.GetJob(ctx, "adopt-issue-100")
	if q.State != job.StateSpecReview {
		t.Fatalf("manual opt-in of an issue must enter issue-review, got %s", q.State)
	}

	// a re-sweep is idempotent (already-known issues are skipped).
	again, err := st.AdoptSweep(ctx, mustSweep(ctx, t, fake), time.Unix(1, 0), now)
	if err != nil {
		t.Fatalf("re-sweep: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a re-sweep must be idempotent, got %v", again)
	}
}

func mustSweep(ctx context.Context, t *testing.T, fake *gh.Fake) gh.BoardSnapshot {
	t.Helper()
	snap, err := fake.BoardSweep(ctx)
	if err != nil {
		t.Fatalf("board sweep: %v", err)
	}
	return snap
}

func hasStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
