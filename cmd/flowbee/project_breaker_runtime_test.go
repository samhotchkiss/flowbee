package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/multirepo"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

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
