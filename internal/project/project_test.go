package project

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
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
