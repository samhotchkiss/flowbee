package acceptance

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/scheduler"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

// These acceptance tests intentionally cross the Phase-2 storage, scheduling,
// decision, breaker, and Driver boundaries. Focused package tests pin each
// component; this file proves that their durable joins remain project-scoped
// when they are composed through a real migrated SQLite store.

func openPhase2Store(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(context.Background(), st.DB); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	return st
}

func createPhase2Project(t *testing.T, st *store.Store, id string, weight int, now time.Time) {
	t.Helper()
	if _, err := st.CreatePortfolioProject(context.Background(), store.PortfolioProject{
		ID: id, Name: strings.ToUpper(id), State: "active", SchedulerWeight: weight,
	}, now); err != nil {
		t.Fatalf("create project %s: %v", id, err)
	}
}

func TestPhase2TwoProjectsReuseHumanEpicSlugWithoutAuthorityCollision(t *testing.T) {
	ctx := context.Background()
	st := openPhase2Store(t, filepath.Join(t.TempDir(), "same-slug.db"))
	defer st.Close()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)

	for _, projectID := range []string{"alpha", "beta"} {
		createPhase2Project(t, st, projectID, 1, now)
		epicID := "epic-" + projectID + "-auth"
		if err := st.AddEpicRun(ctx, store.EpicRun{
			ID: epicID, ProjectID: projectID, Slug: "auth",
			AdmissionKey: "intent:" + projectID + ":1", Repo: "repo-" + projectID,
			FilePath: "epics/auth.md", Title: "Authentication",
			Branch: "epic/" + projectID + "/auth", TmuxName: "epic-" + projectID + "-auth",
		}, 1, now); err != nil {
			t.Fatalf("admit %s/auth: %v", projectID, err)
		}
	}

	alpha, err := st.GetEpicRun(ctx, "epic-alpha-auth")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := st.GetEpicRun(ctx, "epic-beta-auth")
	if err != nil {
		t.Fatal(err)
	}
	if alpha.ProjectID != "alpha" || beta.ProjectID != "beta" || alpha.Branch == beta.Branch || alpha.TmuxName == beta.TmuxName {
		t.Fatalf("same-slug authority collided: alpha=%+v beta=%+v", alpha, beta)
	}
	var deliveries, crossProject int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_deliveries WHERE epic_id IN (?,?)`, alpha.ID, beta.ID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_deliveries d JOIN epics e ON e.id=d.epic_id
		WHERE d.project_id<>e.project_id`).Scan(&crossProject); err != nil {
		t.Fatal(err)
	}
	if deliveries != 2 || crossProject != 0 {
		t.Fatalf("delivery ownership deliveries=%d cross_project=%d", deliveries, crossProject)
	}
}

func TestPhase2DecisionResponseCannotAdvanceAnotherProject(t *testing.T) {
	ctx := context.Background()
	st := openPhase2Store(t, filepath.Join(t.TempDir(), "decision-isolation.db"))
	defer st.Close()
	now := time.Date(2026, 7, 19, 20, 30, 0, 0, time.UTC)
	hashA := "sha256:" + strings.Repeat("a", 64)
	hashB := "sha256:" + strings.Repeat("b", 64)
	for _, fixture := range []struct {
		project, request, hash string
	}{{"alpha", "decision-alpha", hashA}, {"beta", "decision-beta", hashB}} {
		createPhase2Project(t, st, fixture.project, 1, now)
		if _, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
			ID: fixture.request, ProjectID: fixture.project, Kind: workintent.DecisionPlanReview,
			Title: "Approve plan", Prompt: "Approve this exact plan?",
			ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseApprove},
			RequestedBy:           "interactor:" + fixture.project, RouteTo: "human:sam",
			SubjectArtifactRef: "artifact:" + fixture.project, SubjectVersion: 1,
			SubjectSHA256: fixture.hash,
		}, now); err != nil {
			t.Fatalf("create %s decision: %v", fixture.project, err)
		}
	}

	// The request ID and artifact fence belong to beta, but the command scope is
	// alpha. The composite project/request predicate must fail before any response,
	// action, or state transition is written.
	_, err := st.RespondDecision(ctx, "alpha", store.DecisionResponseInput{
		RequestID: "decision-beta", RequestVersion: 1, SubjectVersion: 1,
		SubjectSHA256: hashB, Kind: workintent.ResponseApprove,
		ActorID: "human:sam", IdempotencyKey: "cross-project-attempt",
	}, now.Add(time.Minute))
	if !errors.Is(err, store.ErrDecisionNotFound) {
		t.Fatalf("cross-project response err=%v, want decision not found", err)
	}
	beta, err := st.GetDecisionRequest(ctx, "beta", "decision-beta")
	if err != nil || beta.State != workintent.RequestOpen || beta.CurrentResponseID != "" {
		t.Fatalf("beta advanced by alpha response: beta=%+v err=%v", beta, err)
	}
	var effects int
	if err := st.DB.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM decision_responses WHERE idempotency_key='cross-project-attempt')+
		(SELECT COUNT(*) FROM decision_response_actions WHERE project_id='beta')`).Scan(&effects); err != nil {
		t.Fatal(err)
	}
	if effects != 0 {
		t.Fatalf("cross-project attempt wrote %d effects", effects)
	}
}

func TestPhase2FairLeaseStateSurvivesRestartWithoutDuplicateEffects(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fair-restart.db")
	now := time.Date(2026, 7, 19, 21, 0, 0, 0, time.UTC)
	st := openPhase2Store(t, path)
	for _, projectID := range []string{"alpha", "beta"} {
		createPhase2Project(t, st, projectID, 1, now)
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: projectID + "-build", ProjectID: projectID, Kind: job.KindBuild,
			Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	claimFairTurn := func(s *store.Store, leaseID, worker string, at time.Time) string {
		t.Helper()
		snapshot, err := s.LoadProjectFairSnapshot(ctx, scheduler.PoolBuild)
		if err != nil {
			t.Fatal(err)
		}
		candidates, err := s.ReadyCandidates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		turn := scheduler.PickProjectFair(candidates, snapshot.Policies, snapshot.Active, snapshot.FairState,
			scheduler.FairConfig{Pool: scheduler.PoolBuild, Now: at})
		if !turn.OK {
			t.Fatal("no fair scheduling turn")
		}
		fair := &store.ProjectFairClaim{Pool: scheduler.PoolBuild, ProjectID: turn.WinningProject,
			JobID: turn.Selected.JobID, ForcedByAge: turn.ForcedByAge,
			NextState: turn.NextState, Decisions: turn.Decisions, Now: at}
		if _, err := s.ClaimReadyJob(ctx, store.ClaimParams{
			JobID: turn.Selected.JobID, LeaseID: leaseID, Identity: worker,
			ModelFamily: "codex", Role: job.RoleEngWorker, TTL: time.Hour, Now: at, Fair: fair,
		}); err != nil {
			t.Fatalf("claim %s: %v", turn.Selected.JobID, err)
		}
		return turn.Selected.JobID
	}

	firstJob := claimFairTurn(st, "lease-first", "builder-one", now)
	if firstJob != "alpha-build" {
		t.Fatalf("deterministic first project=%s", firstJob)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = openPhase2Store(t, path)
	defer st.Close()
	first, err := st.GetJob(ctx, firstJob)
	if err != nil || first.State != job.StateLeased || first.LeaseID != "lease-first" {
		t.Fatalf("lease lost after restart: job=%+v err=%v", first, err)
	}
	if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{
		JobID: firstJob, LeaseID: "lease-duplicate", Identity: "builder-zombie",
		ModelFamily: "codex", Role: job.RoleEngWorker, TTL: time.Hour, Now: now.Add(time.Minute),
	}); err == nil {
		t.Fatal("restart allowed duplicate lease effect")
	}
	secondJob := claimFairTurn(st, "lease-second", "builder-two", now.Add(2*time.Minute))
	if secondJob != "beta-build" {
		t.Fatalf("durable fair credit did not select beta after restart: %s", secondJob)
	}
	var turns, liveLeases int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_scheduler_turns`).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM leases WHERE ended_at IS NULL`).Scan(&liveLeases); err != nil {
		t.Fatal(err)
	}
	if turns != 2 || liveLeases != 2 {
		t.Fatalf("duplicate or lost effects after restart: turns=%d live_leases=%d", turns, liveLeases)
	}
}

func TestPhase2ProjectBreakerDoesNotStallAnotherProject(t *testing.T) {
	ctx := context.Background()
	st := openPhase2Store(t, filepath.Join(t.TempDir(), "breaker-isolation.db"))
	defer st.Close()
	now := time.Date(2026, 7, 19, 21, 30, 0, 0, time.UTC)
	for _, projectID := range []string{"alpha", "beta"} {
		createPhase2Project(t, st, projectID, 1, now)
		repoID := "repo-" + projectID
		if err := st.RegisterRepo(ctx, store.Repo{ID: repoID, Owner: "acme", Repo: repoID, Active: true}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddProjectRepo(ctx, projectID, repoID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: projectID + "-build", ProjectID: projectID, Kind: job.KindBuild,
			Flow: "build", Stage: "build", Role: job.RoleEngWorker, Repo: repoID, Now: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{
		ProjectID: "alpha", RepoID: "repo-alpha", Kind: "ci_outage",
		Reason: "required checks unavailable", RetryAfter: time.Minute,
		EvidenceRef: "github:repo-alpha:checks:503",
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	candidates, err := st.ReadyCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.LoadProjectFairSnapshot(ctx, scheduler.PoolBuild)
	if err != nil {
		t.Fatal(err)
	}
	turn := scheduler.PickProjectFair(candidates, snapshot.Policies, snapshot.Active, snapshot.FairState,
		scheduler.FairConfig{Pool: scheduler.PoolBuild, Now: now.Add(2 * time.Second)})
	if !turn.OK || turn.Selected.JobID != "beta-build" {
		t.Fatalf("breaker in alpha stalled or won shared scheduler: turn=%+v candidates=%+v", turn, candidates)
	}
}

func TestPhase2SessionIncarnationReplacementFencesOldDriverAuthority(t *testing.T) {
	ctx := context.Background()
	st := openPhase2Store(t, filepath.Join(t.TempDir(), "driver-fence.db"))
	defer st.Close()
	st.EnableDriverControlOrigin = true // future-capability fake transport
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	createPhase2Project(t, st, "alpha", 1, now)
	stamp := now.Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
		VALUES ('driver-alpha','host-alpha','store-alpha','boot-alpha','live',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,last_event_id,active,updated_at)
		VALUES ('store-alpha','driver-alpha','cursor-5',5,0,'event-5',1,?)`, stamp); err != nil {
		t.Fatal(err)
	}
	control, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "alpha", WorkerIdentity: store.DriverControlIdentity, Role: store.DriverControlRole,
		HostID: "host-alpha", StoreID: "store-alpha", TmuxServerInstanceID: "server-alpha",
		LifecycleKey: "flowbee-control", TargetEpoch: 1, ProfileID: "flowbee-control",
		WorkspaceRootID: "root-alpha", WorkspaceRelativePath: "flowbee",
		SessionID: "control-alpha", PaneInstanceID: "control-pane-alpha", AgentRunID: "control-run-alpha",
	}, now)
	if err != nil || control.BindingID == "" {
		t.Fatalf("control binding=%+v err=%v", control, err)
	}
	old, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "alpha", WorkerIdentity: "interactor:alpha", Role: store.DriverInteractorRole,
		HostID: "host-alpha", StoreID: "store-alpha", TmuxServerInstanceID: "server-alpha",
		LifecycleKey: "interactor-alpha", TargetEpoch: 1, ProfileID: "interactor",
		WorkspaceRootID: "root-alpha", WorkspaceRelativePath: "project-alpha",
		SessionID: "interactor-alpha-v1", PaneInstanceID: "pane-alpha-v1", AgentRunID: "run-alpha-v1",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := st.CreateConversationThread(ctx, store.CreateConversationThreadInput{
		ID: "conversation-alpha", ProjectID: "alpha", ConversationKey: "primary", Title: "Alpha",
		InteractorActorID: "interactor:alpha", InteractorBindingID: old.BindingID,
		InteractorIncarnationID: old.AgentRunID, IdempotencyKey: "create-alpha-thread",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	message, err := st.AppendConversationMessage(ctx, store.AppendConversationMessageInput{
		ID: "message-alpha", ProjectID: "alpha", ThreadID: thread.ID, Role: "human",
		ActorID: "human:sam", ContentText: "Continue the project", IdempotencyKey: "message-alpha-1",
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report, err := st.ReconcileConversationMessageActions(ctx, now.Add(2*time.Minute)); err != nil || report.ActionsCreated != 1 {
		t.Fatalf("materialize old route=%+v err=%v", report, err)
	}
	if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "alpha", WorkerIdentity: "interactor:alpha", Role: store.DriverInteractorRole,
		HostID: "host-alpha", StoreID: "store-alpha", TmuxServerInstanceID: "server-alpha",
		LifecycleKey: "interactor-alpha", TargetEpoch: 2, ProfileID: "interactor",
		WorkspaceRootID: "root-alpha", WorkspaceRelativePath: "project-alpha",
		SessionID: "interactor-alpha-v2", PaneInstanceID: "pane-alpha-v2", AgentRunID: "run-alpha-v2",
	}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	fake := driver.NewFake()
	runtime := driver.ConversationRuntime{Port: fake, Store: driver.ConversationSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: driver.ConversationStageEvidence{DB: st.DB}, Owner: "phase2-acceptance"}
	report, err := runtime.Tick(ctx, now.Add(4*time.Minute))
	if err != nil || report.Fenced != 1 || fake.SendCalls != 0 || len(fake.Grants) != 0 {
		t.Fatalf("stale authority reached Driver: report=%+v sends=%d grants=%d err=%v",
			report, fake.SendCalls, len(fake.Grants), err)
	}
	if reconcile, err := st.ReconcileConversationMessageActions(ctx, now.Add(5*time.Minute)); err != nil || reconcile.ActionsCreated != 1 {
		t.Fatalf("replacement route not materialized: report=%+v err=%v", reconcile, err)
	}
	var oldFenced, newPending, wrongProject int
	if err := st.DB.QueryRowContext(ctx, `SELECT
		SUM(CASE WHEN state='fenced' THEN 1 ELSE 0 END),
		SUM(CASE WHEN state='pending' THEN 1 ELSE 0 END),
		SUM(CASE WHEN project_id<>'alpha' THEN 1 ELSE 0 END)
		FROM conversation_message_actions WHERE message_id=?`, message.ID).
		Scan(&oldFenced, &newPending, &wrongProject); err != nil {
		t.Fatal(err)
	}
	if oldFenced != 1 || newPending != 1 || wrongProject != 0 {
		t.Fatalf("replacement action history fenced=%d pending=%d wrong_project=%d", oldFenced, newPending, wrongProject)
	}
}
