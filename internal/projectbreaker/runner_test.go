package projectbreaker_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/multirepo"
	"github.com/samhotchkiss/flowbee/internal/projectbreaker"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

type probeReply struct {
	result projectbreaker.ProbeResult
	err    error
	panic  any
}

type deterministicProbe struct {
	replies map[string]probeReply
	calls   []projectbreaker.ProbeRequest
}

func (p *deterministicProbe) Probe(_ context.Context, req projectbreaker.ProbeRequest) (projectbreaker.ProbeResult, error) {
	p.calls = append(p.calls, req)
	reply := p.replies[req.ProjectID+"/"+req.RepoID]
	if reply.panic != nil {
		panic(reply.panic)
	}
	return reply.result, reply.err
}

func seed(t *testing.T, st *store.Store, projectID, repoID string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: projectID, Name: projectID}, now); err != nil {
		t.Fatal(err)
	}
	if repoID != "" {
		if err := st.RegisterRepo(ctx, store.Repo{ID: repoID, Owner: "acme", Repo: repoID, Active: true}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddProjectRepo(ctx, projectID, repoID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{
		ProjectID: projectID, RepoID: repoID, Kind: "github_error", Reason: "dependency unavailable",
		RetryAfter: time.Second, EvidenceRef: "github:failure:" + projectID + ":" + repoID,
	}, now); err != nil {
		t.Fatal(err)
	}
}

func runner(st *store.Store, probe projectbreaker.DependencyProbe, now time.Time) projectbreaker.Runner {
	return projectbreaker.Runner{Store: st, Probe: probe, Config: projectbreaker.Config{
		Owner: "breaker-probe-loop", ClaimTTL: time.Minute, FailureRetryAfter: 2 * time.Minute,
		Budget: 20, Now: func() time.Time { return now },
	}}
}

func TestActionFailureHalfOpenIgnoresFreshQueuedWorkButRequiresFailedEffects(t *testing.T) {
	t0 := time.Date(2026, 7, 19, 20, 45, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		mutate    func(*testing.T, *store.Store, int64)
		recovered bool
	}{
		{name: "fresh unrelated queued work", recovered: true},
		{name: "abandoned effect", mutate: func(t *testing.T, st *store.Store, id int64) {
			t.Helper()
			if err := st.MarkOutboxSuppressed(context.Background(), id); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "sent without audit", mutate: func(t *testing.T, st *store.Store, id int64) {
			t.Helper()
			if _, err := st.DB.ExecContext(context.Background(), `UPDATE outbox SET status='sent' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testutil.NewStore(t)
			seed(t, st, "alpha", "repo-a", t0)
			if _, err := st.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{
				ProjectID: "alpha", RepoID: "repo-a", Kind: "action_failure", Reason: "delivery failed",
				RetryAfter: time.Second, EvidenceRef: "action:failed-effect",
			}, t0); err != nil {
				t.Fatal(err)
			}
			if _, err := st.SeedJob(ctx, store.SeedParams{ID: "queued-action", ProjectID: "alpha", Repo: "repo-a",
				Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: t0}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.EnqueuePROpen(ctx, "queued-action", "head-queued", "main"); err != nil {
				t.Fatal(err)
			}
			row, ok, err := st.NextPendingOutboxForRepo(ctx, "repo-a")
			if err != nil || !ok {
				t.Fatalf("row=%+v ok=%v err=%v", row, ok, err)
			}
			if tc.mutate != nil {
				tc.mutate(t, st, row.ID)
			}

			now := t0.Add(2 * time.Second)
			repositories := &repositoryFactFixture{facts: map[string]multirepo.RepositoryProbeFacts{
				"repo-a": {RepoID: "repo-a", Fingerprint: "healthy-repository-read"},
			}}
			probe := projectbreaker.MechanicalDependencyProbe{
				Projects: st, Repositories: repositories, Actions: st, Now: func() time.Time { return now },
			}
			report, err := runner(st, probe, now).RunOnce(ctx)
			if err != nil || report.Claimed != 1 {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			breaker, err := st.GetProjectBreaker(ctx, "alpha", "repo-a")
			if err != nil {
				t.Fatal(err)
			}
			if tc.recovered {
				if report.Recovered != 1 || breaker.State != "closed" {
					t.Fatalf("fresh queued work blocked recovery: report=%+v breaker=%+v", report, breaker)
				}
			} else if report.Reopened != 1 || breaker.State != "open" {
				t.Fatalf("failed effect falsely recovered: report=%+v breaker=%+v", report, breaker)
			}
		})
	}
}

func TestRunOnceClosesOnlyWithMechanicalRecoveryEvidence(t *testing.T) {
	st := testutil.NewStore(t)
	t0 := time.Date(2026, 7, 19, 21, 0, 0, 0, time.UTC)
	seed(t, st, "alpha", "repo-a", t0)
	now := t0.Add(2 * time.Second)
	fake := &deterministicProbe{replies: map[string]probeReply{
		"alpha/repo-a": {result: projectbreaker.ProbeResult{
			Recovered: true, EvidenceKind: "github_api_read", EvidenceRef: "github:repo-a:request-99", ObservedAt: now,
		}},
	}}
	report, err := runner(st, fake, now).RunOnce(context.Background())
	if err != nil || report.Claimed != 1 || report.Recovered != 1 || report.Reopened != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	got, err := st.GetProjectBreaker(context.Background(), "alpha", "repo-a")
	if err != nil || got.State != "closed" || got.LastRecoveryFact != "github:repo-a:request-99" {
		t.Fatalf("breaker=%+v err=%v", got, err)
	}
	if len(fake.calls) != 1 || fake.calls[0].FailureKind != "github_error" || fake.calls[0].ProbeEpoch != 1 {
		t.Fatalf("probe request=%+v", fake.calls)
	}
}

func TestRunOnceExpectedFailureReopensWithProbeRetry(t *testing.T) {
	st := testutil.NewStore(t)
	t0 := time.Date(2026, 7, 19, 21, 30, 0, 0, time.UTC)
	seed(t, st, "alpha", "repo-a", t0)
	now := t0.Add(2 * time.Second)
	fake := &deterministicProbe{replies: map[string]probeReply{
		"alpha/repo-a": {result: projectbreaker.ProbeResult{
			FailureReason: "GitHub still returns 503", RetryAfter: 3 * time.Minute,
		}},
	}}
	report, err := runner(st, fake, now).RunOnce(context.Background())
	if err != nil || report.Claimed != 1 || report.Reopened != 1 || report.Poisoned != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	got, err := st.GetProjectBreaker(context.Background(), "alpha", "repo-a")
	if err != nil || got.State != "open" || got.Reason != "GitHub still returns 503" || !got.ProbeDueAt.Equal(now.Add(3*time.Minute)) {
		t.Fatalf("breaker=%+v err=%v", got, err)
	}
}

func TestRunOncePoisonScopeCannotStopHealthyScope(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	seed(t, st, "alpha", "repo-a", t0)
	seed(t, st, "beta", "repo-b", t0)
	now := t0.Add(2 * time.Second)
	fake := &deterministicProbe{replies: map[string]probeReply{
		"alpha/repo-a": {panic: "corrupt credential fixture"},
		"beta/repo-b": {result: projectbreaker.ProbeResult{
			Recovered: true, EvidenceKind: "github_api_read", EvidenceRef: "github:repo-b:healthy", ObservedAt: now,
		}},
	}}
	report, err := runner(st, fake, now).RunOnce(ctx)
	if err != nil || report.Claimed != 2 || report.Poisoned != 1 || report.Recovered != 1 || len(report.Outcomes) != 2 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	alpha, err := st.GetProjectBreaker(ctx, "alpha", "repo-a")
	if err != nil || alpha.State != "open" || alpha.Reason != "mechanical probe unavailable" {
		t.Fatalf("poison breaker=%+v err=%v", alpha, err)
	}
	beta, err := st.GetProjectBreaker(ctx, "beta", "repo-b")
	if err != nil || beta.State != "closed" {
		t.Fatalf("healthy breaker=%+v err=%v", beta, err)
	}
	var poisonState string
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM reconciler_poison_facts
		WHERE reconciler_name='project_breaker_probe' AND fact_key='project:alpha/repo:repo-a'`).Scan(&poisonState); err != nil || poisonState != "open" {
		t.Fatalf("poison state=%q err=%v", poisonState, err)
	}
	if len(fake.calls) != 2 || fake.calls[1].ProjectID != "beta" {
		t.Fatalf("poison stopped batch calls=%+v", fake.calls)
	}
}

func TestRunnerRestartReclaimsExpiredClaimAndFencesDeadOwner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "breaker-runner-restart.db")
	t0 := time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC)
	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, first.DB); err != nil {
		t.Fatal(err)
	}
	seed(t, first, "alpha", "repo-a", t0)
	deadClaim, err := first.ReconcileDueProjectBreakerProbes(ctx, "dead-process", t0.Add(time.Second), time.Minute, 1)
	if err != nil || len(deadClaim) != 1 {
		t.Fatalf("dead claim=%+v err=%v", deadClaim, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	now := t0.Add(2 * time.Minute)
	fake := &deterministicProbe{replies: map[string]probeReply{
		"alpha/repo-a": {result: projectbreaker.ProbeResult{
			Recovered: true, EvidenceKind: "github_api_read", EvidenceRef: "github:after-restart", ObservedAt: now,
		}},
	}}
	r := runner(second, fake, now)
	r.Config.Owner = "restarted-process"
	report, err := r.RunOnce(ctx)
	if err != nil || report.Claimed != 1 || report.Recovered != 1 || report.Outcomes[0].Epoch != deadClaim[0].Epoch+1 {
		t.Fatalf("restart report=%+v err=%v", report, err)
	}
	_, err = second.CompleteProjectBreakerProbe(ctx, deadClaim[0], true, store.ProjectBreakerRecoveryFact{
		Kind: "late", EvidenceRef: "late:dead-owner", ObservedAt: t0.Add(time.Minute),
	}, "", 0, now.Add(time.Second))
	if !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("dead owner completion err=%v, want stale epoch", err)
	}
	got, err := second.GetProjectBreaker(ctx, "alpha", "repo-a")
	if err != nil || got.State != "closed" || got.ProbeEpoch != 2 {
		t.Fatalf("post-restart breaker=%+v err=%v", got, err)
	}
}

func TestMalformedSuccessIsPoisonAndNeverClosesBreaker(t *testing.T) {
	st := testutil.NewStore(t)
	t0 := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	seed(t, st, "alpha", "repo-a", t0)
	now := t0.Add(2 * time.Second)
	fake := &deterministicProbe{replies: map[string]probeReply{
		"alpha/repo-a": {result: projectbreaker.ProbeResult{Recovered: true, EvidenceKind: "agent_said_ok", ObservedAt: now}},
	}}
	report, err := runner(st, fake, now).RunOnce(context.Background())
	if err != nil || report.Poisoned != 1 || report.Recovered != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	got, getErr := st.GetProjectBreaker(context.Background(), "alpha", "repo-a")
	if getErr != nil || got.State != "open" || got.LastRecoveryFact != "" {
		t.Fatalf("malformed success closed breaker: %+v err=%v", got, getErr)
	}
}
