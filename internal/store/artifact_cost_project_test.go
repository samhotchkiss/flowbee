package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func seedArtifactCostProject(t *testing.T, st *store.Store, projectID, repoID string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: projectID, Name: projectID}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: repoID, Owner: "fixture", Repo: repoID, Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, projectID, repoID, now); err != nil {
		t.Fatal(err)
	}
}

func claimArtifactCostJob(t *testing.T, st *store.Store, jobID string, now time.Time) int {
	t.Helper()
	claimed, err := st.ClaimReadyJob(context.Background(), store.ClaimParams{
		JobID: jobID, LeaseID: "lease-" + jobID, Identity: "worker-" + jobID,
		ModelFamily: "codex", Role: job.RoleEngWorker,
		Attested: []string{"role:eng_worker", "model_family:codex"}, TTL: time.Hour, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return claimed.Epoch
}

func TestArtifactAndCostReadsAreExactProjectScoped(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC)
	seedArtifactCostProject(t, st, "alpha", "repo-alpha", now)
	seedArtifactCostProject(t, st, "beta", "repo-beta", now)

	for _, projectID := range []string{"alpha", "beta"} {
		repoID := "repo-" + projectID
		epicID := projectID + "-epic"
		if err := st.AddEpicRun(ctx, store.EpicRun{ID: epicID, ProjectID: projectID,
			Slug: "shared-label", AdmissionKey: projectID + ":shared", Repo: repoID,
			Branch: "epic/shared-label"}, 1, now); err != nil {
			t.Fatal(err)
		}
		if err := st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{
			EpicID: epicID, ProjectID: projectID, Repo: repoID, Branch: "epic/shared-label",
			PRNumber: 7, PROpen: true, HeadSHA: "head-" + projectID, BaseSHA: "base",
			CIState: "pending",
		}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SeedJob(ctx, store.SeedParams{ID: projectID + "-job", ProjectID: projectID,
			Kind: job.KindBuild, Flow: "build", FlowID: "shared-flow", Stage: "build",
			Role: job.RoleEngWorker, Now: now}); err != nil {
			t.Fatal(err)
		}
		epoch := claimArtifactCostJob(t, st, projectID+"-job", now)
		if _, err := st.RecordCost(ctx, store.CostParams{JobID: projectID + "-job", ProjectID: projectID,
			Epoch: epoch, Now: now, TokensInDelta: 10, MicroUSDDelta: 100}); err != nil {
			t.Fatal(err)
		}
	}

	alphaArtifacts, err := st.ListEpicArtifactsForProject(ctx, "alpha")
	if err != nil || len(alphaArtifacts) != 1 || alphaArtifacts[0].EpicID != "alpha-epic" ||
		alphaArtifacts[0].ProjectID != "alpha" || alphaArtifacts[0].HeadSHA != "head-alpha" {
		t.Fatalf("alpha artifacts=%+v err=%v", alphaArtifacts, err)
	}
	if _, err := st.GetEpicArtifactForProject(ctx, "alpha", "beta-epic"); !errors.Is(err, store.ErrEpicRunNotFound) {
		t.Fatalf("cross-project artifact read err=%v", err)
	}
	alphaCosts, err := st.AllJobCostForProject(ctx, "alpha")
	if err != nil || len(alphaCosts) != 1 || alphaCosts[0].ProjectID != "alpha" || alphaCosts[0].JobID != "alpha-job" {
		t.Fatalf("alpha costs=%+v err=%v", alphaCosts, err)
	}
	rollup, err := st.FlowCostRollupForProject(ctx, "alpha", "shared-flow")
	if err != nil || len(rollup.Jobs) != 1 || rollup.Jobs[0].JobID != "alpha-job" || rollup.TotalMicroUSD != 100 {
		t.Fatalf("alpha rollup=%+v err=%v", rollup, err)
	}
}

func TestArtifactAndCostWritersDeriveOwnerAndRejectSpoofOrMove(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 23, 15, 0, 0, time.UTC)
	seedArtifactCostProject(t, st, "alpha", "repo-alpha", now)
	seedArtifactCostProject(t, st, "beta", "repo-beta", now)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "alpha-epic", ProjectID: "alpha",
		Slug: "artifact", AdmissionKey: "alpha:artifact", Repo: "repo-alpha", Branch: "epic/artifact"}, 1, now); err != nil {
		t.Fatal(err)
	}
	spoof := store.EpicArtifactFact{EpicID: "alpha-epic", ProjectID: "beta", Repo: "repo-alpha",
		Branch: "epic/artifact", PRNumber: 8, PROpen: true, HeadSHA: "head", BaseSHA: "base", CIState: "pending"}
	if err := st.ObserveEpicArtifactFact(ctx, spoof, now); !errors.Is(err, store.ErrEpicArtifactProjectMismatch) {
		t.Fatalf("artifact project spoof err=%v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_artifacts SET project_id='beta' WHERE epic_id='alpha-epic'`); err == nil {
		t.Fatal("artifact ownership moved across projects")
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epics SET project_id='beta' WHERE id='alpha-epic'`); err == nil {
		t.Fatal("artifact parent ownership moved across projects")
	}

	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "alpha-job", ProjectID: "alpha",
		Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
		t.Fatal(err)
	}
	epoch := claimArtifactCostJob(t, st, "alpha-job", now)
	if _, err := st.RecordCost(ctx, store.CostParams{JobID: "alpha-job", ProjectID: "beta",
		Epoch: epoch, Now: now, MicroUSDDelta: 1}); !errors.Is(err, store.ErrCostProjectMismatch) {
		t.Fatalf("cost project spoof err=%v", err)
	}
	if _, err := st.RecordCost(ctx, store.CostParams{JobID: "alpha-job", ProjectID: "alpha",
		Epoch: epoch, Now: now, MicroUSDDelta: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET project_id='beta' WHERE id='alpha-job'`); err == nil {
		t.Fatal("metered cost parent ownership moved across projects")
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO job_events
		(job_id,project_id,job_seq,kind,actor,payload,created_at)
		VALUES ('alpha-job','beta',99,'cost_metered','spoof','{}',?)`, now.Format(time.RFC3339Nano)); err == nil {
		t.Fatal("cross-project cost replay event was accepted")
	}
}
