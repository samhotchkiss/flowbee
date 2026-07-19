package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/scheduler"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestReadyCandidatesCarryDurableProjectAndPool proves the SQL projection used
// by the Phase-2 fairness shadow does not infer project ownership from repo/CWD
// and labels the role-isolated pool explicitly.
func TestReadyCandidatesCarryDurableProjectAndPool(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(70_000, 0)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{
		ID: "mail", Name: "Mail", State: "active", SchedulerWeight: 2,
	}, now); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "mail-build", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, Now: now,
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET project_id='mail' WHERE id='mail-build'`); err != nil {
		t.Fatalf("assign project fixture: %v", err)
	}

	candidates, err := st.ReadyCandidates(ctx)
	if err != nil {
		t.Fatalf("ready candidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates=%+v", candidates)
	}
	if candidates[0].ProjectID != "mail" || candidates[0].Pool != scheduler.PoolBuild {
		t.Fatalf("candidate lost durable ownership/pool: %+v", candidates[0])
	}
}
