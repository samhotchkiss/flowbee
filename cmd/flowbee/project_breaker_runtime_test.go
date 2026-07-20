package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/multirepo"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func TestPartialMultiRepoSweepDoesNotPoisonGlobalGitHubHealth(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(50_000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{}, "test")
	partialErr := &multirepo.RepoLoopFailures{Operation: "sweep", Failures: []multirepo.RepoLoopFailure{
		{RepoID: "repo-a", Err: errors.New("repo-a unavailable")},
	}}
	recordMultiRepoGitHubSweep(srv, map[string]int{"repo-b": 0}, partialErr)
	rec := httptest.NewRecorder()
	srv.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if strings.Contains(rec.Body.String(), "repo-a unavailable") {
		t.Fatalf("partial success was presented as a global GitHub outage: %s", rec.Body.String())
	}
	recordMultiRepoGitHubSweep(srv, map[string]int{}, partialErr)
	rec = httptest.NewRecorder()
	srv.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !strings.Contains(rec.Body.String(), "repo-a unavailable") {
		t.Fatalf("all-repo failure was not surfaced globally: %s", rec.Body.String())
	}
}

func TestProductionDrainFailureRequiresRetryThresholdAndMechanicallyRecovers(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Now().UTC().Add(-2 * time.Minute)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "alpha", Name: "Alpha"}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "repo-a", Owner: "acme", Repo: "repo-a", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, "alpha", "repo-a", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "alpha-out", ProjectID: "alpha", Repo: "repo-a",
		Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueuePROpen(ctx, "alpha-out", "head-a", "main"); err != nil {
		t.Fatal(err)
	}
	row, ok, err := st.NextPendingOutboxForRepo(ctx, "repo-a")
	if err != nil || !ok {
		t.Fatalf("outbox row=%+v ok=%v err=%v", row, ok, err)
	}
	passErr := &multirepo.RepoLoopFailures{Operation: "drain", Failures: []multirepo.RepoLoopFailure{
		{RepoID: "repo-a", Err: errors.New("temporary GitHub 503")},
	}}
	if err := st.BumpOutboxAttempts(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := observeMultiRepoLoopFailures(ctx, st, passErr, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetProjectBreaker(ctx, "alpha", "repo-a"); !errors.Is(err, store.ErrProjectBreakerNotFound) {
		t.Fatalf("one-shot write failure opened breaker: %v", err)
	}
	if err := st.BumpOutboxAttempts(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.BumpOutboxAttempts(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := observeMultiRepoLoopFailures(ctx, st, passErr, now); err != nil {
		t.Fatal(err)
	}
	breaker, err := st.GetProjectBreaker(ctx, "alpha", "repo-a")
	if err != nil || breaker.FailureKind != "action_failure" {
		t.Fatalf("threshold breaker=%+v err=%v", breaker, err)
	}
	if err := st.MarkOutboxSent(ctx, row.ID, "mechanically delivered"); err != nil {
		t.Fatal(err)
	}
	st.EnableEpicReviewHandoffV2 = true
	fake := gh.NewFake()
	mgr, err := multirepo.New(ctx, st, clock.Real{}, nil,
		func(store.Repo) (gh.Client, gh.Writer, error) { return fake, fake, nil })
	if err != nil {
		t.Fatal(err)
	}
	report, err := newProductionProjectBreakerRunner(st, mgr, "action-recovery-test").RunOnce(ctx)
	if err != nil || report.Recovered < 1 {
		t.Fatalf("action recovery report=%+v err=%v", report, err)
	}
	breaker, err = st.GetProjectBreaker(ctx, "alpha", "repo-a")
	if err != nil || breaker.State != "closed" || !strings.HasPrefix(breaker.LastRecoveryFact, "project:alpha/repo-a@sha256:") {
		t.Fatalf("action breaker did not mechanically close: %+v err=%v", breaker, err)
	}
}

func TestProductionMultiRepoFailureObservationIsScopedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	t0 := time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC)
	for _, item := range []struct{ project, repo string }{{"alpha", "repo-a"}, {"beta", "repo-b"}} {
		if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: item.project, Name: item.project}, t0); err != nil {
			t.Fatal(err)
		}
		if err := st.RegisterRepo(ctx, store.Repo{ID: item.repo, Owner: "acme", Repo: item.repo, Active: true}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddProjectRepo(ctx, item.project, item.repo, t0); err != nil {
			t.Fatal(err)
		}
	}
	passErr := &multirepo.RepoLoopFailures{Operation: "sweep", Failures: []multirepo.RepoLoopFailure{
		{RepoID: "repo-a", Err: errors.New("installation token denied")},
	}}
	if err := observeMultiRepoLoopFailures(ctx, st, passErr, t0); err != nil {
		t.Fatal(err)
	}
	if err := observeMultiRepoLoopFailures(ctx, st, passErr, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, projectID := range []string{"alpha", "default"} {
		breaker, err := st.GetProjectBreaker(ctx, projectID, "repo-a")
		if err != nil || breaker.FailureKind != "github_error" || breaker.StateVersion != 1 || breaker.FailureCount != 1 {
			t.Fatalf("%s/repo-a breaker=%+v err=%v", projectID, breaker, err)
		}
		events, err := st.ListProjectBreakerEvents(ctx, projectID, "repo-a")
		if err != nil || len(events) != 1 || !strings.HasPrefix(events[0].EvidenceRef, "multirepo:sweep:repo-a@sha256:") {
			t.Fatalf("%s/repo-a events=%+v err=%v", projectID, events, err)
		}
	}
	if _, err := st.GetProjectBreaker(ctx, "beta", "repo-b"); !errors.Is(err, store.ErrProjectBreakerNotFound) {
		t.Fatalf("repo-a failure escaped into beta/repo-b: %v", err)
	}
}

func TestProductionProjectBreakerRunnerUsesMultiRepoReadBoundary(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "alpha", Name: "Alpha"}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "repo-a", Owner: "acme", Repo: "alpha", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, "alpha", "repo-a", now); err != nil {
		t.Fatal(err)
	}
	st.EnableEpicReviewHandoffV2 = true
	fake := gh.NewFake()
	mgr, err := multirepo.New(ctx, st, clock.Real{}, nil,
		func(store.Repo) (gh.Client, gh.Writer, error) { return fake, fake, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{
		ProjectID: "alpha", RepoID: "repo-a", Kind: "github_error", Reason: "GitHub unavailable",
		RetryAfter: time.Second, EvidenceRef: "incident:github",
	}, now); err != nil {
		t.Fatal(err)
	}

	report, err := newProductionProjectBreakerRunner(st, mgr, "serve-test").RunOnce(ctx)
	if err != nil || report.Claimed != 1 || report.Recovered != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	breaker, err := st.GetProjectBreaker(ctx, "alpha", "repo-a")
	if err != nil || breaker.State != "closed" ||
		!strings.HasPrefix(breaker.LastRecoveryFact, "project:alpha/repo-a@sha256:") {
		t.Fatalf("breaker=%+v err=%v", breaker, err)
	}
	if calls := fake.Calls(); len(calls) != 1 || calls[0] != "BoardSweep" {
		t.Fatalf("production breaker probe escaped read-only GitHub boundary: %v", calls)
	}
	events, err := st.ListProjectBreakerEvents(ctx, "alpha", "repo-a")
	if err != nil || len(events) < 3 {
		t.Fatalf("immutable breaker evidence events=%+v err=%v", events, err)
	}
	last := events[len(events)-1]
	if last.Kind != "probe_recovered" || last.ActorKind != "reconciler" ||
		last.EvidenceRef != breaker.LastRecoveryFact {
		t.Fatalf("recovery fact was not appended immutably: %+v", last)
	}
}
