package store_test

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestRetryAbandonedOutbox re-arms a job's abandoned outbox actions to `pending` (only the
// abandoned ones, only for that job) and leaves already-sent/pending rows untouched.
func TestRetryAbandonedOutbox(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)

	exec := func(jobID, action, head, status string) {
		if _, err := st.DB.ExecContext(ctx,
			`INSERT INTO outbox (job_id, action, head_sha, status) VALUES (?,?,?,?)`,
			jobID, action, head, status); err != nil {
			t.Fatal(err)
		}
	}
	exec("j", "issues.create", "h1", "abandoned")
	exec("j", "pulls.create", "h2", "abandoned")
	exec("j", "pulls.comment", "h3", "sent")          // already done — must NOT be re-armed
	exec("other", "issues.create", "h4", "abandoned") // a different job — untouched

	n, err := st.RetryAbandonedOutbox(ctx, "j")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n != 2 {
		t.Fatalf("re-armed %d, want 2 (the two abandoned rows for job j)", n)
	}
	count := func(jobID, status string) int {
		var c int
		_ = st.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM outbox WHERE job_id=? AND status=?`, jobID, status).Scan(&c)
		return c
	}
	if count("j", "pending") != 2 {
		t.Errorf("job j pending=%d, want 2 (re-armed)", count("j", "pending"))
	}
	if count("j", "sent") != 1 {
		t.Errorf("the already-sent row must be untouched; sent=%d want 1", count("j", "sent"))
	}
	if count("other", "abandoned") != 1 {
		t.Errorf("a different job's abandoned row must be untouched; got %d", count("other", "abandoned"))
	}

	// idempotent: nothing abandoned now -> 0 re-armed.
	if n2, _ := st.RetryAbandonedOutbox(ctx, "j"); n2 != 0 {
		t.Errorf("second call re-armed %d, want 0 (nothing abandoned)", n2)
	}
}
