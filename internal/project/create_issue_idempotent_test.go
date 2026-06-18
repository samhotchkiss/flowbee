package project

import (
	"context"
	"testing"
)

// TestCreateIssueIdempotentOnResend: a re-sent ActionCreateIssue row for a job whose
// issue was ALREADY materialized (a CP crash between StampIssueNumber and the row being
// marked sent) must NOT create a duplicate GitHub issue — the handler sees the stamped
// issue_number and consumes the row without calling CreateIssue again.
func TestCreateIssueIdempotentOnResend(t *testing.T) {
	st, fake, sender, clk := newSender(t)
	ctx := context.Background()

	// a spec job that already materialized issue #42 (the post-stamp, pre-marked-sent
	// state a crash leaves behind).
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO jobs (id, kind, flow, stage, state, role, blocked_by, required_capabilities,
		                  enqueued_at, lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq)
		VALUES ('spec1', 'spec', 'spec', 'author', 'spec_authoring', 'spec_author', '[]', '[]',
		        datetime('now'), 0, 0, 5, 0, 3, 1)`); err != nil {
		t.Fatalf("seed spec: %v", err)
	}
	if err := st.StampIssueNumber(ctx, "spec1", 42, clk.Now()); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	// the still-pending ActionCreateIssue row (the re-send).
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO outbox (job_id, action, head_sha, payload, status)
		VALUES ('spec1', 'issues.create', '', '{}', 'pending')`); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if _, err := sender.DrainOnce(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	for _, c := range fake.Calls() {
		if c == "CreateIssue" {
			t.Fatal("re-send created a DUPLICATE GitHub issue (idempotency guard missing)")
		}
	}
	if n := len(fake.Issues()); n != 0 {
		t.Fatalf("no new issue should be created on the re-send, got %d", n)
	}
}
