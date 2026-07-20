package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
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

func TestRepoFailureProjectionIsExactAndEvidenceIdempotent(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 19, 19, 30, 0, 0, time.UTC)
	seedBreakerProject(t, st, "alpha", "repo-a")
	seedBreakerProject(t, st, "beta", "repo-b")

	first, err := st.RecordProjectBreakerFailureForRepo(ctx, "repo-a", "github_error",
		"repository sweep failed", time.Minute, "multirepo:sweep:repo-a@sha256:same", t0)
	if err != nil {
		t.Fatal(err)
	}
	// RegisterRepo explicitly binds every legacy repo to the default project; the
	// additional alpha binding is equally authoritative. Beta is not a repo-a
	// owner and must not receive the failure.
	if len(first) != 2 || first[0].ProjectID != "alpha" || first[1].ProjectID != "default" {
		t.Fatalf("repo-a projection scopes=%+v, want alpha and default in stable order", first)
	}
	second, err := st.RecordProjectBreakerFailureForRepo(ctx, "repo-a", "github_error",
		"repository sweep failed again", time.Minute, "multirepo:sweep:repo-a@sha256:same", t0.Add(10*time.Second))
	if err != nil || len(second) != 2 {
		t.Fatalf("repeat=%+v err=%v", second, err)
	}
	for _, projectID := range []string{"alpha", "default"} {
		breaker, err := st.GetProjectBreaker(ctx, projectID, "repo-a")
		if err != nil || breaker.StateVersion != 1 || breaker.FailureCount != 1 {
			t.Fatalf("repeat evidence stormed %s breaker: %+v err=%v", projectID, breaker, err)
		}
		events, err := st.ListProjectBreakerEvents(ctx, projectID, "repo-a")
		if err != nil || len(events) != 1 || events[0].EvidenceRef != "multirepo:sweep:repo-a@sha256:same" {
			t.Fatalf("repeat evidence stormed %s ledger: %+v err=%v", projectID, events, err)
		}
	}
	if _, err := st.GetProjectBreaker(ctx, "beta", "repo-b"); !errors.Is(err, store.ErrProjectBreakerNotFound) {
		t.Fatalf("repo-a failure escaped into beta/repo-b: %v", err)
	}

	third, err := st.RecordProjectBreakerFailureForRepo(ctx, "repo-a", "github_error",
		"new repository failure", time.Minute, "multirepo:sweep:repo-a@sha256:new", t0.Add(time.Minute))
	if err != nil || len(third) != 2 || third[0].StateVersion != 2 || third[0].FailureCount != 2 {
		t.Fatalf("new evidence did not refresh exact breaker: %+v err=%v", third, err)
	}
}

func TestRepositoryActionProbeRequiresAuditedSendAndRejectsAbandonedOrInflight(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 19, 45, 0, 0, time.UTC)
	seedBreakerProject(t, st, "alpha", "repo-a")
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "action-probe", ProjectID: "alpha", Repo: "repo-a",
		Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueuePROpen(ctx, "action-probe", "head-action", "main"); err != nil {
		t.Fatal(err)
	}
	row, ok, err := st.NextPendingOutboxForRepo(ctx, "repo-a")
	if err != nil || !ok {
		t.Fatalf("row=%+v ok=%v err=%v", row, ok, err)
	}
	assertProbe := func(want int, label string) {
		t.Helper()
		unresolved, _, fingerprint, err := st.ReadRepositoryActionProbe(ctx, "repo-a")
		if err != nil || unresolved != want || fingerprint == "" {
			t.Fatalf("%s unresolved=%d fingerprint=%q err=%v, want %d", label, unresolved, fingerprint, err, want)
		}
	}
	assertProbe(0, "fresh unrelated queued work")
	if err := st.BumpOutboxAttempts(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	assertProbe(1, "retried pending")
	if err := st.MarkOutboxSuppressed(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	assertProbe(1, "abandoned without effect")
	if _, err := st.DB.ExecContext(ctx, `UPDATE outbox SET status='delivering' WHERE id=?`, row.ID); err != nil {
		t.Fatal(err)
	}
	assertProbe(1, "future in-flight status")
	if _, err := st.DB.ExecContext(ctx, `UPDATE outbox SET status='sent' WHERE id=?`, row.ID); err != nil {
		t.Fatal(err)
	}
	assertProbe(1, "sent without audit")
	if _, err := st.DB.ExecContext(ctx, `UPDATE outbox SET status='pending',attempts=0 WHERE id=?`, row.ID); err != nil {
		t.Fatal(err)
	}
	assertProbe(0, "pending with no attempts is neutral")
	if err := st.MarkOutboxSent(ctx, row.ID, "verified effect"); err != nil {
		t.Fatal(err)
	}
	assertProbe(0, "audited send")
}

func TestProjectBreakerProbeLifecycleFactsAndFences(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	seedBreakerProject(t, st, "alpha", "repo-a")
	seedBreakerProject(t, st, "beta")
	opened, err := st.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{
		ProjectID: "alpha", RepoID: "repo-a", Kind: "merge_incident", Reason: "merge API 503",
		RetryAfter: 2 * time.Minute, EvidenceRef: "github:merge:503",
	}, t0)
	if err != nil || opened.State != "open" || opened.StateVersion != 1 {
		t.Fatalf("opened=%+v err=%v", opened, err)
	}
	// The active dedup key is only unique inside one project. A malformed or
	// independently produced item in another project must never be updated or
	// resolved by alpha's breaker lifecycle.
	stamp := t0.UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO attention_items
		(id,project_id,kind,state,dedup_key,occurrences,first_seen_at,last_seen_at,created_at,updated_at)
		VALUES ('beta-same-breaker-dedup','beta','needs_input','open',
		'project_breaker:alpha:repo-a',1,?,?,?,?)`, stamp, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
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
	var betaState string
	var betaOccurrences int
	if err := st.DB.QueryRowContext(ctx, `SELECT state,occurrences FROM attention_items
		WHERE project_id='beta' AND id='beta-same-breaker-dedup'`).Scan(&betaState, &betaOccurrences); err != nil {
		t.Fatal(err)
	}
	if betaState != "open" || betaOccurrences != 1 {
		t.Fatalf("alpha breaker mutated beta attention state=%q occurrences=%d", betaState, betaOccurrences)
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
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM attention_items
		WHERE project_id='beta' AND id='beta-same-breaker-dedup'`).Scan(&betaState); err != nil || betaState != "open" {
		t.Fatalf("alpha breaker recovery resolved beta attention state=%q err=%v", betaState, err)
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
