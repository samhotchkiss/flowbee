package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestMergeHandoffViewListsApprovedPRs: the merge_handoff lane lists each approved-
// but-human-merges change with its PR number (the operator's merge queue), and
// excludes jobs in other states.
func TestMergeHandoffViewListsApprovedPRs(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	// a merge_handoff job with an open PR.
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "h", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		Repo: "flowbee", Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='merge_handoff', issue_number=42 WHERE id='h'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO domain_b_facts (job_id, pr_exists, pr_number, ci_green, merged, updated_at)
		VALUES ('h', 1, 77, 1, 0, ?)`, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// a job in another state must NOT appear.
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "d", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='done' WHERE id='d'`); err != nil {
		t.Fatal(err)
	}

	rows, err := st.MergeHandoffView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("MergeHandoffView returned %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.JobID != "h" || r.Repo != "flowbee" || r.IssueNumber != 42 || r.PRNumber != 77 {
		t.Fatalf("row = %+v, want {h flowbee 42 77}", r)
	}
}
