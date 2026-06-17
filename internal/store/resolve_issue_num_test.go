package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestResolveIssueNum(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	// Case 1: build job with issue_number stamped directly (adopted issue).
	t.Run("adopted issue", func(t *testing.T) {
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: "adopted-build", Kind: job.KindBuild, Flow: "build", Stage: "build",
			Role: job.RoleEngWorker, BaseSHA: "sha1", Now: now,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := st.StampIssueNumber(ctx, "adopted-build", 42, now); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		if got := st.ResolveIssueNum(ctx, "adopted-build"); got != 42 {
			t.Fatalf("want 42, got %d", got)
		}
	})

	// Case 2: spec-flow build — FlowID points at a spec job that carries the issue number.
	t.Run("spec flow", func(t *testing.T) {
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: "spec-job", Kind: job.KindSpec, Flow: "spec", Stage: "spec",
			Role: job.RoleSpecAuthor, BaseSHA: "sha2", Now: now,
		}); err != nil {
			t.Fatalf("seed spec: %v", err)
		}
		if err := st.StampIssueNumber(ctx, "spec-job", 99, now); err != nil {
			t.Fatalf("stamp spec: %v", err)
		}
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: "flow-build", Kind: job.KindBuild, Flow: "build", Stage: "build",
			Role: job.RoleEngWorker, BaseSHA: "sha2", FlowID: "spec-job", Now: now,
		}); err != nil {
			t.Fatalf("seed build: %v", err)
		}
		if got := st.ResolveIssueNum(ctx, "flow-build"); got != 99 {
			t.Fatalf("want 99, got %d", got)
		}
	})

	// Case 3: no issue bound — returns 0.
	t.Run("no issue", func(t *testing.T) {
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: "no-issue-job", Kind: job.KindBuild, Flow: "build", Stage: "build",
			Role: job.RoleEngWorker, BaseSHA: "sha3", Now: now,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if got := st.ResolveIssueNum(ctx, "no-issue-job"); got != 0 {
			t.Fatalf("want 0, got %d", got)
		}
	})
}
