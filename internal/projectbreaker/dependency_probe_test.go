package projectbreaker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/multirepo"
	"github.com/samhotchkiss/flowbee/internal/projectbreaker"
)

type projectRepoFixture map[string][]string

func (f projectRepoFixture) ProjectRepoIDs(_ context.Context, projectID string, _ bool) ([]string, error) {
	return append([]string(nil), f[projectID]...), nil
}

type repositoryFactFixture struct {
	facts map[string]multirepo.RepositoryProbeFacts
	errs  map[string]error
	calls []string
}

func (f *repositoryFactFixture) ReadRepositoryProbe(_ context.Context, repoID string) (multirepo.RepositoryProbeFacts, error) {
	f.calls = append(f.calls, repoID)
	return f.facts[repoID], f.errs[repoID]
}

func TestMechanicalDependencyProbeUsesExactRepoAndImmutableHashEvidence(t *testing.T) {
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	repos := &repositoryFactFixture{facts: map[string]multirepo.RepositoryProbeFacts{
		"repo-a": {RepoID: "repo-a", Fingerprint: "abc123"},
		"repo-b": {RepoID: "repo-b", Fingerprint: "not-called"},
	}}
	probe := projectbreaker.MechanicalDependencyProbe{
		Projects: projectRepoFixture{"alpha": {"repo-a", "repo-b"}}, Repositories: repos,
		Now: func() time.Time { return now }, RetryAfter: 2 * time.Minute,
	}
	result, err := probe.Probe(context.Background(), projectbreaker.ProbeRequest{
		ProjectID: "alpha", RepoID: "repo-a", FailureKind: "github_error",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Recovered || result.EvidenceKind != "github_board_read" ||
		result.EvidenceRef != "project:alpha/repo-a@sha256:abc123" || !result.ObservedAt.Equal(now) {
		t.Fatalf("result=%+v", result)
	}
	if len(repos.calls) != 1 || repos.calls[0] != "repo-a" {
		t.Fatalf("repo-scoped probe widened scope: %v", repos.calls)
	}
}

func TestMechanicalDependencyProbeProjectScopeRequiresEveryRepo(t *testing.T) {
	repos := &repositoryFactFixture{facts: map[string]multirepo.RepositoryProbeFacts{
		"repo-a": {RepoID: "repo-a", Fingerprint: "a", GreenPullRequests: 1},
		"repo-b": {RepoID: "repo-b", Fingerprint: "b", GreenPullRequests: 0},
	}}
	probe := projectbreaker.MechanicalDependencyProbe{
		Projects: projectRepoFixture{"alpha": {"repo-b", "repo-a"}}, Repositories: repos,
		Now: func() time.Time { return time.Unix(100, 0).UTC() },
	}
	result, err := probe.Probe(context.Background(), projectbreaker.ProbeRequest{ProjectID: "alpha", FailureKind: "ci_outage"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Recovered || result.RetryAfter <= 0 || result.FailureReason == "" {
		t.Fatalf("unproven project-wide CI recovery passed: %+v", result)
	}
	if len(repos.calls) != 2 || repos.calls[0] != "repo-a" || repos.calls[1] != "repo-b" {
		t.Fatalf("project repositories were not probed in stable order: %v", repos.calls)
	}
}

func TestMechanicalDependencyProbeReadFailureIsExpectedOpenNotPoison(t *testing.T) {
	repos := &repositoryFactFixture{
		facts: map[string]multirepo.RepositoryProbeFacts{}, errs: map[string]error{"repo-a": errors.New("temporary 503")},
	}
	probe := projectbreaker.MechanicalDependencyProbe{
		Projects: projectRepoFixture{"alpha": {"repo-a"}}, Repositories: repos, RetryAfter: time.Minute,
	}
	result, err := probe.Probe(context.Background(), projectbreaker.ProbeRequest{ProjectID: "alpha", FailureKind: "github_error"})
	if err != nil {
		t.Fatalf("ordinary dependency outage must reopen its scope, not poison the runner: %v", err)
	}
	if result.Recovered || result.RetryAfter != time.Minute || result.FailureReason == "" {
		t.Fatalf("result=%+v", result)
	}
}

func TestMechanicalDependencyProbeNeverClosesEffectBreakerFromReadReceipt(t *testing.T) {
	repos := &repositoryFactFixture{facts: map[string]multirepo.RepositoryProbeFacts{
		"repo-a": {RepoID: "repo-a", Fingerprint: "healthy-read"},
	}}
	probe := projectbreaker.MechanicalDependencyProbe{Projects: projectRepoFixture{}, Repositories: repos}
	for _, kind := range []string{"merge_incident", "action_failure", "policy_violation"} {
		result, err := probe.Probe(context.Background(), projectbreaker.ProbeRequest{ProjectID: "alpha", RepoID: "repo-a", FailureKind: kind})
		if err != nil || result.Recovered || result.FailureReason == "" {
			t.Fatalf("kind=%s result=%+v err=%v", kind, result, err)
		}
	}
}
