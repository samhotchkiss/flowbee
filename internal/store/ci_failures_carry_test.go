package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestCIFailBouncePersistsFailingChecks: when a build's CI is definitively red and the
// job bounces back to build, the NAMES of the failed checks are persisted on the job
// (last_ci_failures) so the rebuild's lease context can tell the agent exactly which gate
// to re-run + fix — the §F compounding-memory read that keeps a re-attempt from rebuilding
// blind and re-failing the same check.
func TestCIFailBouncePersistsFailingChecks(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='review_pending', required_capabilities='["role:code_reviewer"]' WHERE id='j'`); err != nil {
		t.Fatal(err)
	}

	failPR := store.ReconciledPR{
		Number: 1, HeadSHA: "h1", BaseSHA: "b1", CIFailed: true,
		FailingChecks: []string{"Architecture and guardrail lints", "golangci-lint"},
	}
	if _, err := st.ApplyReconciledPR(ctx, "j", failPR, now); err != nil {
		t.Fatal(err)
	}

	j, err := st.GetJob(ctx, "j")
	if err != nil {
		t.Fatal(err)
	}
	if j.State != job.StateReady {
		t.Fatalf("state=%s, want ready (re-armed build)", j.State)
	}
	for _, want := range []string{"Architecture and guardrail lints", "golangci-lint"} {
		if !strings.Contains(j.LastCIFailures, want) {
			t.Fatalf("last_ci_failures=%q missing %q", j.LastCIFailures, want)
		}
	}
}

// A CI-fail bounce with NO named checks (older reconcile, or a rollup that reported a
// failed aggregate without itemized contexts) must not clobber the column to empty in a
// way that breaks — it simply leaves it blank, and the rebuild falls back to the generic
// "CI was red" guidance.
func TestCIFailBounceNoNamedChecksLeavesBlank(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "j", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='review_pending', required_capabilities='["role:code_reviewer"]' WHERE id='j'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyReconciledPR(ctx, "j",
		store.ReconciledPR{Number: 1, HeadSHA: "h1", BaseSHA: "b1", CIFailed: true}, now); err != nil {
		t.Fatal(err)
	}
	j, err := st.GetJob(ctx, "j")
	if err != nil {
		t.Fatal(err)
	}
	if j.LastCIFailures != "" {
		t.Fatalf("last_ci_failures=%q, want empty when no checks named", j.LastCIFailures)
	}
}
