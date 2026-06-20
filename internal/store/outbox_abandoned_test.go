package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestOutboxAbandonedExcludesTerminalJobs: the "abandoned GitHub writes" alarm must count only
// ACTIONABLE abandons — those whose owning job is still live (or parked at needs_human). An
// abandon for a job that since reached done/cancelled is benign (a stale-SHA void or a
// superseded merge attempt) and must NOT page, else the alarm is a permanent false positive
// that never drains (russ #215). AbandonedOutbox still LISTS every row, flagged actionable.
func TestOutboxAbandonedExcludesTerminalJobs(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	// three jobs each with an abandoned merge-enqueue row: one still live (review_pending),
	// one done, one cancelled.
	seed := func(id, state string) {
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
			Role: job.RoleEngWorker, Repo: "flowbee", Now: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state=? WHERE id=?`, state, id); err != nil {
			t.Fatalf("set state %s: %v", id, err)
		}
		if err := st.EnqueueOutbox(ctx, store.OutboxRow{
			JobID: id, Action: store.ActionEnqueueMerge, HeadSHA: "sha-" + id,
		}); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE outbox SET status='abandoned' WHERE job_id=?`, id); err != nil {
			t.Fatalf("abandon %s: %v", id, err)
		}
	}
	seed("live", "review_pending")
	seed("merged", string(job.StateDone))
	seed("killed", string(job.StateCancelled))

	// the alarm counts only the live job's abandon.
	counts, err := st.OutboxAbandonedByAction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := counts[store.ActionEnqueueMerge]; got != 1 {
		t.Fatalf("alarm count = %d, want 1 (only the live job's abandon is actionable)", got)
	}

	// the view lists all three, flagging which are actionable.
	rows, err := st.AbandonedOutbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("AbandonedOutbox returned %d rows, want 3", len(rows))
	}
	actionable := map[string]bool{}
	for _, r := range rows {
		actionable[r.JobID] = r.Actionable
	}
	if !actionable["live"] {
		t.Error("the review_pending job's abandon must be actionable")
	}
	if actionable["merged"] || actionable["killed"] {
		t.Errorf("done/cancelled jobs' abandons must be benign, got merged=%v killed=%v",
			actionable["merged"], actionable["killed"])
	}
}
