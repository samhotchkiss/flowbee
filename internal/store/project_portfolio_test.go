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

func TestPhase2DefaultProjectBackfillAndProjectOwnership(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	if err := st.RegisterRepo(ctx, store.Repo{ID: "shared", Owner: "acme", Repo: "shared", Active: true}); err != nil {
		t.Fatal(err)
	}
	defaultRepos, err := st.ProjectRepoIDs(ctx, "default", true)
	if err != nil || len(defaultRepos) != 1 || defaultRepos[0] != "shared" {
		t.Fatalf("default project registration backfill=%v err=%v", defaultRepos, err)
	}
	project, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{
		ID: "mail", Name: "Mail", State: "active", Priority: 20, SchedulerWeight: 3,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if project.StateVersion != 1 || project.SchedulerWeight != 3 {
		t.Fatalf("project=%+v", project)
	}
	if err := st.AddProjectRepo(ctx, "mail", "shared", now); err != nil {
		t.Fatal(err)
	}
	ids, err := st.ProjectRepoIDs(ctx, "mail", true)
	if err != nil || len(ids) != 1 || ids[0] != "shared" {
		t.Fatalf("repos=%v err=%v", ids, err)
	}
	interactor, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "mail", Role: store.DriverInteractorRole, ActorID: "interactor-mail",
	}, now)
	if err != nil || interactor.ActorID != "interactor-mail" {
		t.Fatalf("interactor=%+v err=%v", interactor, err)
	}
	replayed, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "mail", Role: store.DriverInteractorRole, ActorID: "interactor-mail",
	}, now.Add(time.Minute))
	if err != nil || replayed.StateVersion != interactor.StateVersion {
		t.Fatalf("exact actor replay advanced state: before=%+v after=%+v err=%v", interactor, replayed, err)
	}
	orchestrator, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "mail", Role: store.DriverOrchestratorRole, ActorID: "orchestrator-mail",
	}, now)
	if err != nil || orchestrator.ActorID != "orchestrator-mail" {
		t.Fatalf("orchestrator=%+v err=%v", orchestrator, err)
	}
	archived, err := st.SetPortfolioProjectState(ctx, "mail", "archived", "project complete", 1, now.Add(time.Hour))
	if err != nil || archived.State != "archived" || archived.ArchivedAt.IsZero() {
		t.Fatalf("archived=%+v err=%v", archived, err)
	}
	if _, err := st.GetProjectActor(ctx, "mail", store.DriverInteractorRole); err != nil {
		t.Fatalf("archive deleted project history: %v", err)
	}
	if _, err := st.SetPortfolioProjectState(ctx, "mail", "active", "", 1, now); !errors.Is(err, store.ErrProjectConflict) {
		t.Fatalf("stale state version accepted: %v", err)
	}
}

func TestPhase2LegacyPipelineRowsAreExplicitlyDefaultOwned(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "legacy-project-job", Kind: job.KindBuild,
		Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var projectID string
	if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM jobs WHERE id='legacy-project-job'`).Scan(&projectID); err != nil || projectID != "default" {
		t.Fatalf("project_id=%q err=%v", projectID, err)
	}
}

func TestPhase2SeededJobAndLedgerCarryExplicitProject(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	seeded, err := st.SeedJob(ctx, store.SeedParams{ID: "mail-build", ProjectID: "mail",
		Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if seeded.ProjectID != "mail" {
		t.Fatalf("job project=%q", seeded.ProjectID)
	}
	var eventProject string
	if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM job_events
		WHERE job_id='mail-build' AND kind='job_created'`).Scan(&eventProject); err != nil {
		t.Fatal(err)
	}
	if eventProject != "mail" {
		t.Fatalf("job event project=%q", eventProject)
	}
}

func TestPhase2DefaultProjectRetainsSQLiteCreationTimestamp(t *testing.T) {
	st := testutil.NewStore(t)
	project, err := st.GetPortfolioProject(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if project.CreatedAt.IsZero() {
		t.Fatal("default project SQLite timestamp was discarded by the read projection")
	}
}
