package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
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

func TestRetryAbandonedOutboxBulkScopes(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.RegisterRepo(ctx, store.Repo{ID: "flowbee", Owner: "o", Repo: "flowbee", Active: true}))
	must(st.RegisterRepo(ctx, store.Repo{ID: "russ", Owner: "o", Repo: "russ", Active: true}))
	mustJob := func(id, repo string) {
		t.Helper()
		_, err := st.SeedJob(ctx, store.SeedParams{
			ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
			Role: job.RoleEngWorker, Repo: repo, TaskText: "x", Now: time.Unix(1000, 0),
		})
		must(err)
	}
	mustJob("f1", "flowbee")
	mustJob("f2", "flowbee")
	mustJob("r1", "russ")

	exec := func(jobID, action, head, status string) {
		if _, err := st.DB.ExecContext(ctx,
			`INSERT INTO outbox (job_id, action, head_sha, status) VALUES (?,?,?,?)`,
			jobID, action, head, status); err != nil {
			t.Fatal(err)
		}
	}
	exec("f1", "issues.create", "h1", "abandoned")
	exec("f2", "pulls.create", "h2", "abandoned")
	exec("r1", "issues.create", "h3", "abandoned")
	exec("r1", "pulls.comment", "h4", "sent")

	n, err := st.RetryAbandonedOutboxForRepo(ctx, "flowbee")
	if err != nil {
		t.Fatalf("retry repo: %v", err)
	}
	if n != 2 {
		t.Fatalf("repo retry re-armed %d, want 2", n)
	}
	count := func(where string, args ...any) int {
		var c int
		_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE `+where, args...).Scan(&c)
		return c
	}
	if count("job_id IN ('f1','f2') AND status='pending'") != 2 {
		t.Fatal("flowbee repo abandons were not re-armed")
	}
	if count("job_id='r1' AND status='abandoned'") != 1 {
		t.Fatal("russ abandon should be untouched by flowbee repo retry")
	}

	n, err = st.RetryAllAbandonedOutbox(ctx)
	if err != nil {
		t.Fatalf("retry all: %v", err)
	}
	if n != 1 {
		t.Fatalf("all retry re-armed %d, want remaining 1", n)
	}
	if count("status='abandoned'") != 0 {
		t.Fatal("all abandoned outbox rows should now be pending or sent")
	}
	if count("status='sent'") != 1 {
		t.Fatal("sent rows must remain untouched")
	}
}
