package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func seedBreakerProject(t *testing.T, st *store.Store, id string, repos ...string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: id, Name: id}, now); err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		if _, err := st.GetRepo(ctx, repo); errors.Is(err, store.ErrRepoNotFound) {
			if err := st.RegisterRepo(ctx, store.Repo{ID: repo, Owner: "acme", Repo: repo, Active: true}); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.AddProjectRepo(ctx, id, repo, now); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProjectBreakerFaultIsolationNeverStopsAnotherProject(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	seedBreakerProject(t, st, "alpha", "repo-a", "repo-a2")
	seedBreakerProject(t, st, "beta", "repo-b")

	if _, err := st.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{
		ProjectID: "alpha", RepoID: "repo-a", Kind: "ci_outage", Reason: "required check missing",
		RetryAfter: time.Minute, EvidenceRef: "github:repo-a:check-run:42",
	}, now); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		project, repo string
		allowed       bool
		scope         string
	}{
		{"alpha", "repo-a", false, "repository"},
		{"alpha", "repo-a2", true, ""},
		{"beta", "repo-b", true, ""},
	}
	for _, tc := range cases {
		got, err := st.ProjectBreakerDisposition(ctx, tc.project, tc.repo)
		if err != nil || got.Allowed != tc.allowed || got.BlockedScope != tc.scope {
			t.Fatalf("disposition %s/%s = %+v err=%v", tc.project, tc.repo, got, err)
		}
	}

	if _, err := st.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{
		ProjectID: "alpha", Kind: "github_error", Reason: "project credential denied",
		RetryAfter: time.Minute, EvidenceRef: "github:installation:alpha",
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ProjectBreakerDisposition(ctx, "alpha", "repo-a2"); err != nil || got.Allowed || got.BlockedScope != "project" {
		t.Fatalf("project breaker did not cover its second repo: %+v err=%v", got, err)
	}
	if got, err := st.ProjectBreakerDisposition(ctx, "beta", "repo-b"); err != nil || !got.Allowed {
		t.Fatalf("alpha breaker escaped into beta: %+v err=%v", got, err)
	}
}

func TestProjectBreakerProbeLifecycleFactsAndFences(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	seedBreakerProject(t, st, "alpha", "repo-a")
	opened, err := st.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{
		ProjectID: "alpha", RepoID: "repo-a", Kind: "merge_incident", Reason: "merge API 503",
		RetryAfter: 2 * time.Minute, EvidenceRef: "github:merge:503",
	}, t0)
	if err != nil || opened.State != "open" || opened.StateVersion != 1 {
		t.Fatalf("opened=%+v err=%v", opened, err)
	}
	if due, err := st.ReconcileDueProjectBreakerProbes(ctx, "breaker-loop", t0.Add(time.Minute), time.Minute, 5); err != nil || len(due) != 0 {
		t.Fatalf("early probes=%+v err=%v", due, err)
	}
	probes, err := st.ReconcileDueProjectBreakerProbes(ctx, "breaker-loop", t0.Add(2*time.Minute), time.Minute, 5)
	if err != nil || len(probes) != 1 || probes[0].Epoch != 1 {
		t.Fatalf("due probes=%+v err=%v", probes, err)
	}
	probe := probes[0]
	if _, err := st.CompleteProjectBreakerProbe(ctx, store.ProjectBreakerProbe{
		ProjectID: probe.ProjectID, RepoID: probe.RepoID, Owner: probe.Owner, Epoch: probe.Epoch + 1,
	}, true, store.ProjectBreakerRecoveryFact{Kind: "github_check", EvidenceRef: "check:green", ObservedAt: t0.Add(2 * time.Minute)}, "", 0, t0.Add(2*time.Minute+time.Second)); !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("stale epoch err=%v", err)
	}
	if _, err := st.CompleteProjectBreakerProbe(ctx, probe, true, store.ProjectBreakerRecoveryFact{}, "", 0, t0.Add(2*time.Minute+time.Second)); !errors.Is(err, store.ErrProjectBreakerInput) {
		t.Fatalf("fact-free close err=%v", err)
	}
	reopened, err := st.CompleteProjectBreakerProbe(ctx, probe, false, store.ProjectBreakerRecoveryFact{}, "still 503", time.Minute, t0.Add(2*time.Minute+time.Second))
	if err != nil || reopened.State != "open" || reopened.StateVersion != 3 {
		t.Fatalf("failed probe=%+v err=%v", reopened, err)
	}
	probes, err = st.ReconcileDueProjectBreakerProbes(ctx, "breaker-loop-2", t0.Add(3*time.Minute+time.Second), time.Minute, 5)
	if err != nil || len(probes) != 1 || probes[0].Epoch != 2 {
		t.Fatalf("second probe=%+v err=%v", probes, err)
	}
	closed, err := st.CompleteProjectBreakerProbe(ctx, probes[0], true, store.ProjectBreakerRecoveryFact{
		Kind: "merge_preflight", EvidenceRef: "github:mergeable:sha-abc", ObservedAt: t0.Add(3 * time.Minute),
	}, "", 0, t0.Add(3*time.Minute+2*time.Second))
	if err != nil || closed.State != "closed" || closed.LastRecoveryFact != "github:mergeable:sha-abc" {
		t.Fatalf("closed=%+v err=%v", closed, err)
	}
	events, err := st.ListProjectBreakerEvents(ctx, "alpha", "repo-a")
	if err != nil || len(events) != 5 || events[len(events)-1].Kind != "probe_recovered" || events[len(events)-1].EvidenceRef == "" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	var activeAttention int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE project_id='alpha'
		AND kind='project_breaker_open' AND state IN ('open','leased','delivering','awaiting_ack')`).Scan(&activeAttention); err != nil || activeAttention != 0 {
		t.Fatalf("active attention=%d err=%v", activeAttention, err)
	}
}

func TestProjectBreakerProbeBudgetIgnoresLiveClaimAndSelectsDueScope(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 19, 20, 30, 0, 0, time.UTC)
	seedBreakerProject(t, st, "alpha", "repo-a", "repo-a2")
	for _, repo := range []string{"repo-a", "repo-a2"} {
		if _, err := st.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{
			ProjectID: "alpha", RepoID: repo, Kind: "ci_outage", Reason: "checks unavailable",
			RetryAfter: time.Second, EvidenceRef: "check:" + repo,
		}, t0); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.ReconcileDueProjectBreakerProbes(ctx, "loop-1", t0.Add(time.Second), time.Hour, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := st.ReconcileDueProjectBreakerProbes(ctx, "loop-2", t0.Add(2*time.Second), time.Hour, 1)
	if err != nil || len(second) != 1 || second[0].RepoID == first[0].RepoID {
		t.Fatalf("live half-open claim starved other due scope: first=%+v second=%+v err=%v", first, second, err)
	}
}

func TestProjectBreakerRestartReclaimsExpiredProbe(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "breaker-restart.db")
	t0 := time.Date(2026, 7, 19, 21, 0, 0, 0, time.UTC)
	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, first.DB); err != nil {
		t.Fatal(err)
	}
	seedBreakerProject(t, first, "alpha", "repo-a")
	if _, err := first.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{
		ProjectID: "alpha", RepoID: "repo-a", Kind: "action_failure", Reason: "delivery failed",
		RetryAfter: time.Second, EvidenceRef: "action:a1",
	}, t0); err != nil {
		t.Fatal(err)
	}
	old, err := first.ReconcileDueProjectBreakerProbes(ctx, "process-1", t0.Add(time.Second), time.Minute, 1)
	if err != nil || len(old) != 1 {
		t.Fatalf("old probe=%+v err=%v", old, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	reclaimed, err := second.ReconcileDueProjectBreakerProbes(ctx, "process-2", t0.Add(2*time.Minute), time.Minute, 1)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].Epoch != old[0].Epoch+1 {
		t.Fatalf("reclaimed=%+v err=%v", reclaimed, err)
	}
	if _, err := second.CompleteProjectBreakerProbe(ctx, old[0], true, store.ProjectBreakerRecoveryFact{
		Kind: "delivery", EvidenceRef: "receipt:late", ObservedAt: t0.Add(time.Minute),
	}, "", 0, t0.Add(2*time.Minute+time.Second)); !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("dead process completion err=%v", err)
	}
	if _, err := second.CompleteProjectBreakerProbe(ctx, reclaimed[0], true, store.ProjectBreakerRecoveryFact{
		Kind: "delivery", EvidenceRef: "receipt:verified", ObservedAt: t0.Add(2 * time.Minute),
	}, "", 0, t0.Add(2*time.Minute+time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestProjectBreakerOperatorOverrideIsAuditedAndCannotForceClose(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	seedBreakerProject(t, st, "alpha", "repo-a")
	opened, err := st.OverrideProjectBreaker(ctx, store.ProjectBreakerOverride{
		ProjectID: "alpha", RepoID: "repo-a", Action: "open", ExpectedVersion: 0,
		ActorID: "sam", Reason: "hold while rotating repo credential", FailureKind: "github_error", ProbeAfter: time.Hour,
	}, t0)
	if err != nil || opened.State != "open" {
		t.Fatalf("opened=%+v err=%v", opened, err)
	}
	if _, err := st.OverrideProjectBreaker(ctx, store.ProjectBreakerOverride{
		ProjectID: "alpha", RepoID: "repo-a", Action: "probe_now", ExpectedVersion: opened.StateVersion - 1,
		ActorID: "sam", Reason: "credential rotated",
	}, t0.Add(time.Minute)); !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("stale override err=%v", err)
	}
	due, err := st.OverrideProjectBreaker(ctx, store.ProjectBreakerOverride{
		ProjectID: "alpha", RepoID: "repo-a", Action: "probe_now", ExpectedVersion: opened.StateVersion,
		ActorID: "sam", Reason: "credential rotated",
	}, t0.Add(time.Minute))
	if err != nil || due.State != "open" || due.StateVersion != opened.StateVersion+1 {
		t.Fatalf("probe-now=%+v err=%v", due, err)
	}
	events, err := st.ListProjectBreakerEvents(ctx, "alpha", "repo-a")
	if err != nil || len(events) != 2 || events[1].ActorKind != "human" || events[1].ActorID != "sam" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}
