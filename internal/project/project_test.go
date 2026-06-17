package project

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func newSender(t *testing.T) (*store.Store, *gh.Fake, *Sender, *clock.Fake) {
	t.Helper()
	st := testutil.NewStore(t)
	fake := gh.NewFake()
	clk := clock.NewFake(time.Unix(1000, 0))
	return st, fake, New(st, fake, clk, nil), clk
}

func seedReviewPending(t *testing.T, st *store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO jobs (id, kind, flow, stage, state, role, blocked_by, required_capabilities,
		                  enqueued_at, lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq)
		VALUES (?, 'build', 'build', 'review', 'review_pending', 'code_reviewer', '[]', '[]',
		        datetime('now'), 0, 0, 5, 0, 3, 1)`, id); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestDrainIdempotentAuditOncePerKey: a drained outbox row writes exactly ONE
// audit entry keyed (job, action, head_sha); re-enqueue + re-drain never duplicates
// the GitHub action OR the audit row (§8.2.2, §3.3).
func TestDrainIdempotentAuditOncePerKey(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	seedReviewPending(t, st, "j1")

	if _, err := st.EnqueuePROpen(ctx, "j1", "sha-1", "main"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// duplicate enqueue of the SAME (job, action, head_sha) is a no-op.
	if _, err := st.EnqueuePROpen(ctx, "j1", "sha-1", "main"); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	n, err := sender.DrainOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("drain n=%d err=%v want 1", n, err)
	}
	// exactly one PR opened.
	calls := 0
	for _, c := range fake.Calls() {
		if c == "OpenPR" {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("OpenPR called %d times want 1", calls)
	}
	audit, _ := st.AuditLog(ctx, "j1")
	if len(audit) != 1 || audit[0].Action != store.ActionOpenPR || audit[0].HeadSHA != "sha-1" {
		t.Fatalf("audit must be one (pulls.create, sha-1) row: %+v", audit)
	}
	// re-enqueue + re-drain cannot duplicate.
	_, _ = st.EnqueuePROpen(ctx, "j1", "sha-1", "main")
	_, _ = sender.DrainOnce(ctx)
	audit2, _ := st.AuditLog(ctx, "j1")
	if len(audit2) != 1 {
		t.Fatalf("re-drain duplicated an audit row: %d", len(audit2))
	}
}

// TestIssueCommentPostsReviewFindings: an issues.comment outbox row drains to a
// single GitHub issue comment on the job's bound issue (build-list §F: the issue is
// the durable record of the review). A job with no bound issue drains as an audited
// no-op — never a stray comment.
func TestIssueCommentPostsReviewFindings(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()

	// a build job carrying an adopted GitHub issue number (issue 42).
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO jobs (id, kind, flow, stage, state, role, issue_number, blocked_by, required_capabilities,
		                  enqueued_at, lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq)
		VALUES ('jc', 'build', 'build', 'review', 'review_pending', 'code_reviewer', 42, '[]', '[]',
		        datetime('now'), 0, 0, 5, 0, 3, 1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := "### 🐝 Flowbee code review — CHANGES REQUESTED 🔁\n\nMissing a test for the error path."
	if _, err := st.EnqueueIssueComment(ctx, "jc", body, "review-e1"); err != nil {
		t.Fatalf("enqueue comment: %v", err)
	}
	if n, err := sender.DrainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("drain n=%d err=%v want 1", n, err)
	}
	if got := fake.Comments(42); len(got) != 1 || got[0] != body {
		t.Fatalf("issue 42 comments = %v, want exactly the findings body", got)
	}

	// a retried submission (same dedupe key) posts no second comment.
	if _, err := st.EnqueueIssueComment(ctx, "jc", body, "review-e1"); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	_, _ = sender.DrainOnce(ctx)
	if got := fake.Comments(42); len(got) != 1 {
		t.Fatalf("retry duplicated the comment: %d", len(got))
	}

	// a NEW review epoch (new key) posts again.
	if _, err := st.EnqueueIssueComment(ctx, "jc", "approved", "review-e2"); err != nil {
		t.Fatalf("enqueue e2: %v", err)
	}
	_, _ = sender.DrainOnce(ctx)
	if got := fake.Comments(42); len(got) != 2 {
		t.Fatalf("new-epoch review should post a second comment: %d", len(got))
	}

	// a job with NO bound issue drains as an audited no-op, not a stray comment.
	seedReviewPending(t, st, "noissue")
	if _, err := st.EnqueueIssueComment(ctx, "noissue", "x", "review-e1"); err != nil {
		t.Fatalf("enqueue noissue: %v", err)
	}
	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain noissue: %v", err)
	}
	comments := 0
	for _, c := range fake.Calls() {
		if len(c) >= 12 && c[:12] == "IssueComment" {
			comments++
		}
	}
	if comments != 2 {
		t.Fatalf("IssueComment API called %d times, want 2 (the no-issue job must not call it)", comments)
	}
}

// TestRetryAfterParksWholeOutbox: a Retry-After parks the WHOLE outbox until the
// clock passes the horizon (§8.2.4).
func TestRetryAfterParksWholeOutbox(t *testing.T) {
	st, fake, sender, clk := newSender(t)
	ctx := context.Background()
	seedReviewPending(t, st, "j1")
	if _, err := st.EnqueuePROpen(ctx, "j1", "sha-1", "main"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	fake.FailNextWriteWithRetryAfter(30 * time.Second)
	if n, _ := sender.DrainOnce(ctx); n != 0 {
		t.Fatalf("a Retry-After must park (0 sent), got %d", n)
	}
	if n, _ := sender.DrainOnce(ctx); n != 0 {
		t.Fatalf("the outbox stays parked")
	}
	clk.Advance(31 * time.Second)
	if n, _ := sender.DrainOnce(ctx); n != 1 {
		t.Fatalf("after the park expires the row must drain, got %d", n)
	}
}

// TestPoisonOutboxRowDeadLetteredNoHeadOfLineWedge: a row whose GitHub write fails
// PERMANENTLY (a 4xx — e.g. the branch was deleted) must not wedge the serialized,
// oldest-first outbox behind it. The sender dead-letters the poison row (surfacing its
// job to a human, since open-PR is critical) and CONTINUES draining — the good row
// behind it still goes out.
func TestPoisonOutboxRowDeadLetteredNoHeadOfLineWedge(t *testing.T) {
	st, fake, sender, _ := newSender(t)
	ctx := context.Background()
	seedReviewPending(t, st, "poison")
	seedReviewPending(t, st, "good")

	// poison's PR-open is enqueued FIRST -> it is the head of the serialized queue.
	if _, err := st.EnqueuePROpen(ctx, "poison", "sha-poison", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueuePROpen(ctx, "good", "sha-good", "main"); err != nil {
		t.Fatal(err)
	}
	// the poison row's OpenPR fails permanently (branch gone -> 422).
	fake.FailNextWriteWith(&gh.ErrGitHub{StatusCode: 422, Method: "POST", Path: "/pulls", Body: "Reference does not exist"})

	n, err := sender.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("a poison row must NOT abort the whole drain: %v", err)
	}
	if n != 1 {
		t.Fatalf("drain sent=%d, want 1 (the good row proceeds past the dead-lettered poison)", n)
	}
	// the poison job is surfaced to a human with the project_out reason.
	pj, _ := st.GetJob(ctx, "poison")
	if pj.State != job.StateNeedsHuman || pj.EscalationReason != string(job.EscalationProjectOut) {
		t.Fatalf("poison job state=%s reason=%q, want needs_human/project_out", pj.State, pj.EscalationReason)
	}
	// the good job's PR-open actually went out (no head-of-line block).
	if audit, _ := st.AuditLog(ctx, "good"); len(audit) != 1 || audit[0].Action != store.ActionOpenPR {
		t.Fatalf("good row must have drained past the poison: audit=%+v", audit)
	}
	// the poison row left NO audit entry (the action never took effect on GitHub).
	if audit, _ := st.AuditLog(ctx, "poison"); len(audit) != 0 {
		t.Fatalf("dead-lettered poison must not write an audit entry: %+v", audit)
	}
}

// TestMergedJobDeletesItsIssueBranch: when reconcile sees a build's PR merged, it
// enqueues a post-merge cleanup that deletes the flowbee/issue-N branch — so the repo
// doesn't accumulate stale flowbee/issue-* branches. Safe: the merge commit keeps the
// branch's commits reachable from main.
func TestMergedJobDeletesItsIssueBranch(t *testing.T) {
	st, fake, sender, clk := newSender(t)
	sender.WithHistory(&fakeHistory{tip: "main-tip"}, "main")
	ctx := context.Background()

	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO jobs (id, kind, flow, stage, state, role, issue_number, blocked_by,
		                  required_capabilities, enqueued_at, lease_epoch, attempts,
		                  max_attempts, bounces, max_bounces, job_seq)
		VALUES ('m','build','build','review','merging','code_reviewer',77,'[]','[]',
		        datetime('now'),0,0,5,0,9,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyReconciledPR(ctx, "m", store.ReconciledPR{
		Number: 77, Merged: true, MergeCommit: "mc", HeadSHA: "h", BaseSHA: "b",
	}, clk.Now()); err != nil {
		t.Fatalf("reconcile merged: %v", err)
	}
	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := fake.DeletedBranches(); len(got) != 1 || got[0] != "flowbee/issue-77" {
		t.Fatalf("DeletedBranches=%v, want [flowbee/issue-77]", got)
	}
}

// TestTransientNotMergeableRetriedNotResolved: a "not mergeable" 405 that CLEARS on a
// retry — GitHub recomputing mergeability after a sibling merge, NOT a real conflict —
// must be retried and merge, never spinning up the conflict_resolver. (The persistent
// case is covered by TestMergeConflictRoutesToResolverAfterFetch.)
func TestTransientNotMergeableRetriedNotResolved(t *testing.T) {
	st, fake, sender, clk := newSender(t)
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "t", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='merging' WHERE id='t'`); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueOutbox(ctx, store.OutboxRow{
		JobID: "t", Action: store.ActionEnqueueMerge, Payload: `{"pr_number":42}`,
	}); err != nil {
		t.Fatal(err)
	}
	// inject ONE transient "not mergeable" (no SetMergeConflict -> not a real conflict).
	fake.FailNextWriteWith(fmt.Errorf("merge 42: %w", gh.ErrMergeConflict))

	_, _ = sender.DrainOnce(ctx) // attempt 1: transient 405 -> retry, must NOT route
	if j, _ := st.GetJob(ctx, "t"); j.State == job.StateResolvingConflict {
		t.Fatal("a transient not-mergeable must NOT route to the resolver while retries remain")
	}
	_, _ = sender.DrainOnce(ctx) // attempt 2: mergeability settled -> merge succeeds

	enq := fake.Enqueued()
	if len(enq) != 1 || enq[0] != 42 {
		t.Fatalf("the merge should succeed on retry once mergeability settles, enqueued=%v", enq)
	}
	if j, _ := st.GetJob(ctx, "t"); j.State == job.StateResolvingConflict {
		t.Fatal("a transient not-mergeable must never route to the resolver")
	}
}
