package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/epicspec"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/scheduler"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

func TestProjectDashboardProjectsDeliveryBreakersThroughputAndEligibilityExactly(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	now := t0.Add(2 * time.Hour)
	for _, projectID := range []string{"alpha", "beta"} {
		if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: projectID, Name: projectID}, t0); err != nil {
			t.Fatal(err)
		}
		repoID := "repo-" + projectID
		if err := st.RegisterRepo(ctx, store.Repo{ID: repoID, Owner: "acme", Repo: repoID, Active: true}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddProjectRepo(ctx, projectID, repoID, t0); err != nil {
			t.Fatal(err)
		}
	}
	states := []string{"awaiting_artifact", "awaiting_ci", "awaiting_review_dispatch", "in_review", "merge_queued", "cleanup_pending", "complete"}
	for i, state := range states {
		id := "alpha-flow-" + string(rune('a'+i))
		if err := st.AddEpicRun(ctx, store.EpicRun{ID: id, ProjectID: "alpha", Repo: "repo-alpha",
			Slug: id, Title: id, Branch: "epic/" + id}, 1, t0); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET state=? WHERE epic_id=?`, state, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "beta-terminal", ProjectID: "beta", Repo: "repo-beta",
		Slug: "beta-terminal", Title: "beta", Branch: "epic/beta"}, 1, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET state='abandoned' WHERE epic_id='beta-terminal'`); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "beta-admitted", ProjectID: "beta", Repo: "repo-beta",
		Slug: "beta-admitted", Title: "beta admitted", Branch: "epic/beta-admitted"}, 1, t0); err != nil {
		t.Fatal(err)
	}

	opened, err := st.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{ProjectID: "alpha", RepoID: "repo-alpha",
		Kind: "github_error", Reason: "repository unavailable", RetryAfter: time.Minute, EvidenceRef: "repo-alpha:503"}, t0)
	if err != nil || opened.State != "open" {
		t.Fatalf("opened=%+v err=%v", opened, err)
	}
	probes, err := st.ReconcileDueProjectBreakerProbes(ctx, "dashboard-test", t0.Add(time.Minute), 5*time.Minute, 1)
	if err != nil || len(probes) != 1 {
		t.Fatalf("probes=%+v err=%v", probes, err)
	}

	// Immutable events inside and outside the documented 24-hour window.
	for _, event := range []struct {
		project, epic, kind, to string
		at                      time.Time
	}{
		{"alpha", "alpha-flow-e", "merge_verified", "cleanup_pending", now.Add(-time.Hour)},
		{"alpha", "alpha-flow-e", "review_handoff_recovered", "review_queued", now.Add(-30 * time.Minute)},
		{"beta", "beta-terminal", "cleanup_complete", "complete", now.Add(-time.Hour)},
		{"beta", "beta-terminal", "merge_verified", "cleanup_pending", now.Add(-25 * time.Hour)},
	} {
		var epicSeq int
		if err := st.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(epic_seq),0)+1 FROM control_events WHERE epic_id=?`, event.epic).Scan(&epicSeq); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO control_events
			(project_id,epic_id,kind,to_state,epic_seq,created_at) VALUES (?,?,?,?,?,?)`,
			event.project, event.epic, event.kind, event.to, epicSeq, event.at.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "alpha-ready", ProjectID: "alpha", Repo: "repo-alpha",
		Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "beta-history", ProjectID: "beta", Repo: "repo-beta",
		Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: t0}); err != nil {
		t.Fatal(err)
	}
	stamp := t0.Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO leases
		(lease_id,job_id,lease_epoch,identity,model_family,granted_at,ttl_s,deadline,ended_at,end_reason)
		VALUES ('beta-history-lease','beta-history',1,'builder','codex',?,60,?,?,'completed')`, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	decisions, _ := json.Marshal([]scheduler.CandidateDecision{{Candidate: scheduler.Candidate{
		JobID: "alpha-ready", ProjectID: "alpha", Pool: scheduler.PoolBuild},
		Code: scheduler.WhyFairTurn, Detail: "project beta won this build pool turn"}})
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO project_scheduler_turns
		(lease_id,pool,project_id,job_id,decisions_json,created_at) VALUES
		('beta-history-lease','build','beta','beta-history',?,?)`, string(decisions), stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='done' WHERE id='beta-history'`); err != nil {
		t.Fatal(err)
	}
	effectDecisions, _ := json.Marshal([]scheduler.CandidateDecision{{Candidate: scheduler.Candidate{
		JobID: "alpha-flow-e", ProjectID: "alpha", Pool: scheduler.PoolBuild},
		Code: scheduler.WhySelected, Detail: "selected by the project weighted-fair turn"}})
	effectAt := t0.Add(61 * time.Minute).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO project_scheduler_effects
		(pool,project_id,resource_kind,resource_id,effect_kind,effect_id,effect_epoch,decisions_json,created_at)
		VALUES ('build','alpha','epic_builder','alpha-flow-e','builder_launch','builder-effect',0,?,?)`,
		string(effectDecisions), effectAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO project_scheduler_state
		(pool,project_id,deficit,last_served_at,state_version,updated_at) VALUES ('build','alpha',0,?,1,?)`, effectAt, effectAt); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ProjectDashboardAt(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]store.ProjectDashboardRow{}
	for _, row := range rows {
		byID[row.Project.ID] = row
	}
	alpha, beta := byID["alpha"], byID["beta"]
	if alpha.Delivery.Total != 7 || alpha.Delivery.AwaitingArtifact != 1 || alpha.Delivery.AwaitingCI != 1 ||
		alpha.Delivery.AwaitingReviewDispatch != 1 || alpha.Delivery.InReview != 1 || alpha.Delivery.Merge != 1 ||
		alpha.Delivery.Cleanup != 1 || alpha.Delivery.Terminal != 1 || beta.Delivery.Terminal != 1 ||
		beta.Delivery.Admitted != 1 || beta.Delivery.Total != 2 {
		t.Fatalf("delivery isolation alpha=%+v beta=%+v", alpha.Delivery, beta.Delivery)
	}
	if len(alpha.Breakers) != 1 || alpha.Breakers[0].Scope != "repository" || !alpha.Breakers[0].HalfOpen ||
		alpha.Breakers[0].RepoID != "repo-alpha" || alpha.Breakers[0].ProbeLeaseExpiresAt.IsZero() || len(beta.Breakers) != 0 {
		t.Fatalf("breaker isolation alpha=%+v beta=%+v", alpha.Breakers, beta.Breakers)
	}
	if alpha.Throughput.WindowSeconds != 24*60*60 || alpha.Throughput.Merged != 1 || alpha.Throughput.Recoveries != 1 ||
		beta.Throughput.Merged != 0 || beta.Throughput.Completed != 1 {
		t.Fatalf("throughput isolation alpha=%+v beta=%+v", alpha.Throughput, beta.Throughput)
	}
	var alphaBuild store.ProjectSchedulerMetric
	for _, metric := range alpha.Scheduler {
		if metric.Pool == scheduler.PoolBuild {
			alphaBuild = metric
		}
	}
	if alphaBuild.EligibilityStatus != "held" || alphaBuild.WhyNotCode != "project_breaker" || alphaBuild.NextEligibleAt.IsZero() ||
		alphaBuild.ServiceTurns != 1 || alphaBuild.PoolServiceTurns != 2 ||
		alphaBuild.LastDecisionCode != string(scheduler.WhySelected) || alphaBuild.LastDecisionAt.IsZero() {
		t.Fatalf("scheduler why-not=%+v", alphaBuild)
	}
	var betaBuild store.ProjectSchedulerMetric
	for _, metric := range beta.Scheduler {
		if metric.Pool == scheduler.PoolBuild {
			betaBuild = metric
		}
	}
	if betaBuild.Eligible != 1 || betaBuild.EligibilityStatus != "eligible" {
		t.Fatalf("v2 admitted epic missing from build eligibility: %+v", betaBuild)
	}
}

func TestProjectDashboardFoldsResidencyHumanDemandAndOldestBlocker(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{
		ID: "mail", Name: "Mail", Priority: 10, SchedulerWeight: 3, ConcurrencyCap: 2,
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "russ", Owner: "acme", Repo: "russ", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, "mail", "russ", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "mail", Role: store.DriverInteractorRole, ActorID: "interactor-mail",
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "mail", Role: store.DriverOrchestratorRole, ActorID: "orchestrator-mail",
	}, now); err != nil {
		t.Fatal(err)
	}
	binding, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "mail", WorkerIdentity: "interactor-mail", Role: store.DriverInteractorRole,
		HostID: "host-mail", StoreID: "store-mail", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "tmux-mail", LifecycleOwnership: "driver_managed",
		LifecycleKey: "interactor-mail", TargetEpoch: 1, ProfileID: "interactor",
		WorkspaceRootID: "mail", WorkspaceRelativePath: "mail", SessionID: "session-mail",
		PaneInstanceID: "pane-mail", AgentRunID: "run-mail", ObservedAt: now.Add(time.Minute),
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	stamp := now.Add(time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
		VALUES ('mail-driver','host-mail','store-mail','boot-mail','live',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_session_projections
		(store_id,session_id,host_id,pane_instance_id,agent_run_id,tmux_server_instance_id,lifecycle,updated_at)
		VALUES ('store-mail','session-mail','host-mail','pane-mail','run-mail','tmux-mail','active',?)`, stamp); err != nil {
		t.Fatal(err)
	}
	for _, epic := range []store.EpicRun{
		{ID: "mail-active", ProjectID: "mail", Repo: "russ", Title: "Active", Branch: "epic/mail-active"},
		{ID: "mail-parked", ProjectID: "mail", Repo: "russ", Title: "Parked", Branch: "epic/mail-parked"},
	} {
		if err := st.AddEpicRun(ctx, epic, 2, now); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkEpicLaunched(ctx, epic.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epics SET seat_id='mail-seat',state='launching' WHERE id='mail-active'`); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEpicStatus(ctx, "mail-parked", epicspec.StatusBlock{
		UpdatedRaw: now.Add(time.Minute).Format(time.RFC3339), State: "done",
	}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpsertAttentionItem(ctx, store.AttentionItem{
		ID: "mail-blocker", EpicID: "mail-active", Kind: "review_dispatch_overdue",
		DedupKey: "mail:review", Priority: 1, Blocking: true, Detail: "review was not dispatched",
	}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("mail-plan"))
	if _, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
		ID: "mail-decision", ProjectID: "mail", Kind: workintent.DecisionQuestion,
		Title: "Choose behavior", Prompt: "Which behavior?", Options: json.RawMessage(`[]`),
		ResponseSchema: json.RawMessage(`{}`), ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseAnswer},
		RequestedBy: "orchestrator:mail", RouteTo: "human:sam", SubjectArtifactRef: "artifact://mail-plan",
		SubjectVersion: 1, SubjectSHA256: "sha256:" + hex.EncodeToString(hash[:]),
	}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ProjectDashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mail *store.ProjectDashboardRow
	for i := range rows {
		if rows[i].Project.ID == "mail" {
			mail = &rows[i]
		}
	}
	if mail == nil {
		t.Fatalf("mail missing from dashboard: %+v", rows)
	}
	if mail.ActiveEpics != 1 || mail.ParkedEpics != 1 || mail.NeedsYou != 1 {
		t.Fatalf("project counts=%+v", *mail)
	}
	if mail.OldestBlocker != "review was not dispatched" || mail.BlockerKind != "review_dispatch_overdue" || mail.BlockedSince.IsZero() {
		t.Fatalf("oldest blocker=%+v", *mail)
	}
	if mail.Interactor.Status != store.ProjectActorReady || mail.Interactor.ActorID != "interactor-mail" ||
		mail.Interactor.BindingID != binding.BindingID || mail.Interactor.AgentRunID != "run-mail" {
		t.Fatalf("interactor route health=%+v", mail.Interactor)
	}
	if mail.Orchestrator.Status != store.ProjectActorRouteAbsent || mail.Orchestrator.ActorID != "orchestrator-mail" ||
		mail.Orchestrator.BindingID != "" {
		t.Fatalf("orchestrator route health=%+v", mail.Orchestrator)
	}
}

func TestProjectDashboardRequiresExactLiveDriverIncarnationForActorReady(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "mail", Role: store.DriverInteractorRole, ActorID: "interactor-mail",
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "mail", WorkerIdentity: "interactor-mail", Role: store.DriverInteractorRole,
		HostID: "host-mail", StoreID: "store-old", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "tmux-old", LifecycleOwnership: "driver_managed",
		LifecycleKey: "interactor-mail", TargetEpoch: 1, ProfileID: "interactor",
		WorkspaceRootID: "mail", WorkspaceRelativePath: "mail", SessionID: "session-old",
		PaneInstanceID: "pane-old", AgentRunID: "run-old", ObservedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
		VALUES ('old-driver','host-mail','store-old','boot-old','live',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_session_projections
		(store_id,session_id,host_id,pane_instance_id,agent_run_id,tmux_server_instance_id,lifecycle,ended_at,updated_at)
		VALUES ('store-old','session-old','host-mail','pane-old','run-old','tmux-old','ended',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ProjectDashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Project.ID == "mail" {
			if row.Interactor.Status != store.ProjectActorRouteAbsent || row.Interactor.ActorID != "interactor-mail" ||
				row.Interactor.BindingID != "" || row.Interactor.SessionID != "" {
				t.Fatalf("ended exact incarnation looked routable: %+v", row.Interactor)
			}
			return
		}
	}
	t.Fatal("mail missing")
}

func TestProjectDashboardShowsAllocationServiceShareAndStarvation(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	for _, project := range []store.PortfolioProject{
		{ID: "mail", Name: "Mail", SchedulerWeight: 3},
		{ID: "calendar", Name: "Calendar", SchedulerWeight: 1},
	} {
		if _, err := st.CreatePortfolioProject(ctx, project, now); err != nil {
			t.Fatal(err)
		}
	}

	seed := func(id, projectID string, enqueued time.Time) {
		t.Helper()
		if _, err := st.SeedJob(ctx, store.SeedParams{ID: id, Kind: job.KindBuild, Flow: "build",
			Stage: "build", Role: job.RoleEngWorker, Now: enqueued}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET project_id=?,enqueued_at=? WHERE id=?`,
			projectID, enqueued.UTC().Format(time.RFC3339Nano), id); err != nil {
			t.Fatal(err)
		}
	}
	seed("mail-active", "mail", now.Add(-time.Minute))
	seed("calendar-active", "calendar", now.Add(-time.Minute))
	for _, claim := range []struct{ jobID, leaseID string }{{"mail-active", "mail-live"}, {"calendar-active", "calendar-live"}} {
		if _, err := st.ClaimReadyJob(ctx, store.ClaimParams{JobID: claim.jobID, LeaseID: claim.leaseID,
			Identity: "builder-" + claim.jobID, ModelFamily: "codex", Role: job.RoleEngWorker,
			TTL: time.Hour, Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	seed("mail-starved", "mail", now.Add(-30*time.Minute))
	seed("calendar-waiting", "calendar", now.Add(-5*time.Minute))

	// Four historical scheduler turns make the service ratio exact and auditable.
	for i, projectID := range []string{"mail", "mail", "mail", "calendar"} {
		jobID, leaseID := projectID+"-history-"+string(rune('a'+i)), "history-"+string(rune('a'+i))
		seed(jobID, projectID, now.Add(-time.Hour))
		stamp := now.Add(-time.Duration(40-i) * time.Minute).UTC().Format(time.RFC3339Nano)
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO leases
			(lease_id,job_id,lease_epoch,identity,model_family,granted_at,ttl_s,deadline,ended_at,end_reason)
			VALUES (?,?,1,'historical-builder','codex',?,60,?,?,'completed')`, leaseID, jobID, stamp, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO project_scheduler_turns
			(lease_id,pool,project_id,job_id,forced_by_age,decisions_json,created_at)
			VALUES (?,'build',?,?,0,'[]',?)`, leaseID, projectID, jobID, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='done',lease_id=NULL WHERE id=?`, jobID); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []struct {
		id     string
		served time.Time
	}{
		{"mail", now.Add(-20 * time.Minute)}, {"calendar", now.Add(-4 * time.Minute)},
	} {
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO project_scheduler_state
			(pool,project_id,deficit,last_served_at,state_version,updated_at) VALUES ('build',?,0,?,1,?)`,
			state.id, state.served.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE projects SET state='archived' WHERE id='default'`); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ProjectDashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store.EvaluateProjectDashboardStarvation(rows, now, 15*time.Minute)
	byID := map[string]store.ProjectDashboardRow{}
	for _, row := range rows {
		byID[row.Project.ID] = row
	}
	mail, calendar := byID["mail"], byID["calendar"]
	if mail.Capacity.Allocated != 1 || mail.Capacity.Build != 1 || calendar.Capacity.Allocated != 1 {
		t.Fatalf("capacity allocation mail=%+v calendar=%+v", mail.Capacity, calendar.Capacity)
	}
	metric := func(row store.ProjectDashboardRow, pool string) store.ProjectSchedulerMetric {
		t.Helper()
		for _, got := range row.Scheduler {
			if got.Pool == pool {
				return got
			}
		}
		t.Fatalf("pool %s missing from %+v", pool, row.Scheduler)
		return store.ProjectSchedulerMetric{}
	}
	mailBuild, calendarBuild := metric(mail, "build"), metric(calendar, "build")
	if mailBuild.ServiceTurns != 3 || mailBuild.PoolServiceTurns != 4 ||
		mailBuild.ServiceShareBasisPoints != 7500 || mailBuild.ConfiguredWeightShareBasisPoints != 7500 {
		t.Fatalf("mail service metric=%+v", mailBuild)
	}
	if calendarBuild.ServiceTurns != 1 || calendarBuild.ServiceShareBasisPoints != 2500 ||
		calendarBuild.ConfiguredWeightShareBasisPoints != 2500 {
		t.Fatalf("calendar service metric=%+v", calendarBuild)
	}
	if !mailBuild.Starved || mailBuild.Eligible != 1 || mailBuild.EligibleWaitSeconds != int64(20*time.Minute/time.Second) ||
		mailBuild.StarvationDueAt != now.Add(-5*time.Minute) {
		t.Fatalf("mail starvation metric=%+v", mailBuild)
	}
	if calendarBuild.Starved || calendarBuild.Eligible != 1 || calendarBuild.EligibleWaitSeconds != int64(4*time.Minute/time.Second) {
		t.Fatalf("calendar starvation metric=%+v", calendarBuild)
	}
}

func TestProjectDashboardMarksBothLogicalActorsUnregisteredByDefault(t *testing.T) {
	st := testutil.NewStore(t)
	rows, err := st.ProjectDashboard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Project.ID != "default" ||
		rows[0].Interactor.Status != store.ProjectActorUnregistered ||
		rows[0].Orchestrator.Status != store.ProjectActorUnregistered {
		t.Fatalf("default actor health=%+v", rows)
	}
}
