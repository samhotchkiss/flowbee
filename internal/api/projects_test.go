package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func TestPhase2PortfolioExposesExactActorRouteHealthAndETag(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"viewer": {{ProjectID: "*", Role: auth.HumanViewer}},
	}, false)
	st, ts := phase2ProjectAPIServer(t, access)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "mail-starved", Kind: job.KindBuild, Flow: "build",
		Stage: "build", Role: job.RoleEngWorker, Now: now.Add(-20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET project_id='mail' WHERE id='mail-starved'`); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []store.ProjectActorRoute{
		{ProjectID: "mail", Role: store.DriverInteractorRole, ActorID: "interactor-mail"},
		{ProjectID: "mail", Role: store.DriverOrchestratorRole, ActorID: "orchestrator-mail"},
	} {
		if _, err := st.RegisterProjectActor(ctx, actor, now); err != nil {
			t.Fatal(err)
		}
	}
	token := signedHumanSession(t, access, "viewer", "csrf-viewer")
	resp, got := humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/portfolio", nil, token, "", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") == "" || got["schema_version"] != "flowbee.portfolio/v1" {
		t.Fatalf("initial portfolio status=%d etag=%q body=%v", resp.StatusCode, resp.Header.Get("ETag"), got)
	}
	oldETag := resp.Header.Get("ETag")

	if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "mail", WorkerIdentity: "interactor-mail", Role: store.DriverInteractorRole,
		HostID: "host-mail", StoreID: "store-mail", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "tmux-mail",
		LifecycleOwnership: "driver_managed",
		LifecycleKey:       "interactor-mail", TargetEpoch: 1, ProfileID: "interactor",
		WorkspaceRootID: "mail", WorkspaceRelativePath: "mail", SessionID: "session-mail",
		PaneInstanceID: "pane-mail", AgentRunID: "run-mail", ObservedAt: now.Add(time.Second),
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	stamp := now.Add(time.Second).UTC().Format(time.RFC3339Nano)
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

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/portfolio", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: auth.HumanSessionCookie, Value: token})
	req.Header.Set("If-None-Match", oldETag)
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") == oldETag {
		t.Fatalf("binding did not advance portfolio digest: status=%d old=%q new=%q", resp.StatusCode, oldETag, resp.Header.Get("ETag"))
	}
	var body struct {
		Projects []store.ProjectDashboardRow `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var mail *store.ProjectDashboardRow
	for i := range body.Projects {
		if body.Projects[i].Project.ID == "mail" {
			mail = &body.Projects[i]
		}
	}
	if mail == nil || mail.Interactor.Status != store.ProjectActorReady ||
		mail.Interactor.AgentRunID != "run-mail" || mail.Orchestrator.Status != store.ProjectActorRouteAbsent {
		t.Fatalf("portfolio actor health=%+v", mail)
	}
	if mail.Breakers == nil || len(mail.Breakers) != 0 || mail.Delivery.Total != 0 ||
		mail.Throughput.WindowSeconds != int64(store.ProjectThroughputWindow/time.Second) || mail.Throughput.Merged != 0 {
		t.Fatalf("portfolio must expose explicit empty durable flow/risk fields: %+v", mail)
	}
	var build *store.ProjectSchedulerMetric
	for i := range mail.Scheduler {
		if mail.Scheduler[i].Pool == "build" {
			build = &mail.Scheduler[i]
		}
	}
	if build == nil || !build.Starved || build.Eligible != 1 || build.EligibleWaitSeconds != 20*60 ||
		build.StarvationBoundSeconds != 15*60 || mail.Capacity.Allocated != 0 {
		t.Fatalf("portfolio fairness/capacity metric=%+v capacity=%+v", build, mail.Capacity)
	}

	unchanged, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/portfolio", nil)
	if err != nil {
		t.Fatal(err)
	}
	unchanged.AddCookie(&http.Cookie{Name: auth.HumanSessionCookie, Value: token})
	unchanged.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	resp304, err := ts.Client().Do(unchanged)
	if err != nil {
		t.Fatal(err)
	}
	resp304.Body.Close()
	if resp304.StatusCode != http.StatusNotModified {
		t.Fatalf("unchanged portfolio status=%d want 304", resp304.StatusCode)
	}
}

func TestPhase2PortfolioRiskAndFlowAreStrictlyProjectScoped(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"viewer": {{ProjectID: "*", Role: auth.HumanViewer}},
	}, false)
	st, ts := phase2ProjectAPIServer(t, access)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	for _, projectID := range []string{"alpha", "beta"} {
		if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: projectID, Name: projectID}, now); err != nil {
			t.Fatal(err)
		}
		repoID := "repo-" + projectID
		if err := st.RegisterRepo(ctx, store.Repo{ID: repoID, Owner: "acme", Repo: repoID, Active: true}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddProjectRepo(ctx, projectID, repoID, now); err != nil {
			t.Fatal(err)
		}
		if err := st.AddEpicRun(ctx, store.EpicRun{ID: projectID + "-epic", ProjectID: projectID,
			Repo: repoID, Slug: projectID + "-epic", Title: projectID, Branch: "epic/" + projectID}, 1, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET state='awaiting_review_dispatch' WHERE epic_id='alpha-epic'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET state='complete' WHERE epic_id='beta-epic'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{ProjectID: "alpha", RepoID: "repo-alpha",
		Kind: "github_error", Reason: "alpha only", RetryAfter: time.Minute, EvidenceRef: "alpha:503"}, now); err != nil {
		t.Fatal(err)
	}
	token := signedHumanSession(t, access, "viewer", "csrf-viewer")
	resp, got := humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/portfolio", nil, token, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("portfolio status=%d", resp.StatusCode)
	}
	var body struct {
		Projects []store.ProjectDashboardRow `json:"projects"`
	}
	raw, _ := json.Marshal(got)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	byID := map[string]store.ProjectDashboardRow{}
	for _, row := range body.Projects {
		byID[row.Project.ID] = row
	}
	alpha, beta := byID["alpha"], byID["beta"]
	if alpha.Delivery.AwaitingReviewDispatch != 1 || len(alpha.Breakers) != 1 ||
		beta.Delivery.Terminal != 1 || beta.Breakers == nil || len(beta.Breakers) != 0 {
		t.Fatalf("portfolio scope leaked alpha=%+v beta=%+v", alpha, beta)
	}
}

func TestPhase2ProjectEpicsAreStrictlyProjectScopedAndEmptyIsArray(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"mail-viewer": {{ProjectID: "mail", Role: auth.HumanViewer}},
	}, false)
	st, ts := phase2ProjectAPIServer(t, access)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	for _, p := range []store.PortfolioProject{{ID: "mail", Name: "Mail"}, {ID: "calendar", Name: "Calendar"}} {
		if _, err := st.CreatePortfolioProject(ctx, p, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]string{{"mail", "mail-repo"}, {"mail", "mail-docs"}, {"calendar", "calendar-repo"}} {
		if err := st.RegisterRepo(ctx, store.Repo{ID: pair[1], Owner: "fixture", Repo: pair[1], Active: true}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddProjectRepo(ctx, pair[0], pair[1], now); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []store.EpicRun{
		{ID: "mail-epic", Slug: "shared-slug", ProjectID: "mail", Repositories: []string{"mail-repo", "mail-docs"}, DeliveryRepo: "mail-repo", Title: "Mail epic", Branch: "dev/mail"},
		{ID: "calendar-epic", Slug: "shared-slug", ProjectID: "calendar", Repo: "calendar-repo", Title: "Calendar epic", Branch: "dev/calendar"},
	} {
		if err := st.AddEpicRun(ctx, e, 1, now); err != nil {
			t.Fatal(err)
		}
	}
	token := signedHumanSession(t, access, "mail-viewer", "csrf-mail")
	resp, got := humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/projects/mail/epics", nil, token, "", "")
	if resp.StatusCode != http.StatusOK || got["schema_version"] != "flowbee.project-epics/v1" {
		t.Fatalf("mail epics status=%d body=%v", resp.StatusCode, got)
	}
	epics, ok := got["epics"].([]any)
	if !ok || len(epics) != 1 || epics[0].(map[string]any)["ID"] != "mail-epic" {
		t.Fatalf("project epic isolation=%v", got["epics"])
	}
	mailEpic := epics[0].(map[string]any)
	if mailEpic["delivery_repo"] != "mail-repo" || mailEpic["repository_set_mode"] != "explicit" {
		t.Fatalf("repository projection=%v", mailEpic)
	}
	repositories, ok := mailEpic["repositories"].([]any)
	if !ok || len(repositories) != 2 || repositories[0] != "mail-docs" || repositories[1] != "mail-repo" {
		t.Fatalf("repository set=%v", mailEpic["repositories"])
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/projects/calendar/epics", nil, token, "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-project epics status=%d want 403", resp.StatusCode)
	}

	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "empty", Name: "Empty"}, now); err != nil {
		t.Fatal(err)
	}
	empty, err := st.ListEpicRunsForProject(ctx, "empty")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty project epics=%v err=%v; want non-nil []", empty, err)
	}
}

func phase2ProjectAPIServer(t *testing.T, access *auth.HumanAccess) (*store.Store, *httptest.Server) {
	t.Helper()
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{HumanAccess: access}, "phase2-project-api-test")
	ts := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(ts.Close)
	return st, ts
}

func TestPhase2PortfolioGrantAndProjectGrantStayDistinct(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"portfolio-admin":  {{ProjectID: "*", Role: auth.HumanAdmin}},
		"portfolio-viewer": {{ProjectID: "*", Role: auth.HumanViewer}},
		"mail-admin":       {{ProjectID: "mail", Role: auth.HumanAdmin}},
	}, false)
	st, ts := phase2ProjectAPIServer(t, access)
	if _, err := st.CreatePortfolioProject(context.Background(), store.PortfolioProject{
		ID: "mail", Name: "Mail",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	resp, _ := humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/projects", nil, "", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous portfolio list status=%d want 401", resp.StatusCode)
	}

	mailToken := signedHumanSession(t, access, "mail-admin", "csrf-mail")
	resp, _ = humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/projects", nil, mailToken, "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("project grant widened to portfolio status=%d want 403", resp.StatusCode)
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/projects/mail", nil, mailToken, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exact project read status=%d want 200", resp.StatusCode)
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/projects/default", nil, mailToken, "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-project read status=%d want 403", resp.StatusCode)
	}

	viewerToken := signedHumanSession(t, access, "portfolio-viewer", "csrf-viewer")
	resp, _ = humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/projects", nil, viewerToken, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("portfolio viewer list status=%d want 200", resp.StatusCode)
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/projects",
		map[string]any{"id": "viewer-created", "name": "Forbidden"}, viewerToken, "csrf-viewer", "viewer-create")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("portfolio viewer create status=%d want 403", resp.StatusCode)
	}
}

func TestPhase2ProjectCreateRequiresCSRFAndPayloadBoundIdempotency(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"admin": {{ProjectID: "*", Role: auth.HumanAdmin}},
	}, false)
	_, ts := phase2ProjectAPIServer(t, access)
	token := signedHumanSession(t, access, "admin", "csrf-admin")
	body := map[string]any{"id": "mail", "name": "Mail", "priority": 20, "scheduler_weight": 3}

	resp, _ := humanRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/projects", body,
		token, "", "create-mail")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("project create without CSRF status=%d want 403", resp.StatusCode)
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/projects", body,
		token, "csrf-admin", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("project create without idempotency key status=%d want 400", resp.StatusCode)
	}
	resp, got := humanRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/projects", body,
		token, "csrf-admin", "create-mail")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("project create status=%d body=%v", resp.StatusCode, got)
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/projects", body,
		token, "csrf-admin", "create-mail")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("exact project create replay status=%d want 201", resp.StatusCode)
	}

	// The key is the operation identity, not decoration. Reusing it for a
	// different project must not create a second durable operation.
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/projects",
		map[string]any{"id": "calendar", "name": "Calendar"}, token, "csrf-admin", "create-mail")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("changed payload under project-create key status=%d want 409", resp.StatusCode)
	}
}

func TestPhase2EveryProjectMutationRequiresIdempotencyKey(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"admin": {{ProjectID: "mail", Role: auth.HumanAdmin}},
	}, false)
	st, ts := phase2ProjectAPIServer(t, access)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "russ", Owner: "acme", Repo: "russ", Active: true}); err != nil {
		t.Fatal(err)
	}
	token := signedHumanSession(t, access, "admin", "csrf-admin")
	tests := []struct {
		name string
		path string
		body any
	}{
		{"state", "/v1/projects/mail/state", map[string]any{"state": "paused", "reason": "maintenance", "expected_state_version": 1}},
		{"repository", "/v1/projects/mail/repos", map[string]any{"repo_id": "russ"}},
		{"actor", "/v1/projects/mail/actors", map[string]any{"role": store.DriverInteractorRole, "actor_id": "interactor-mail"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, _ := humanRequest(t, ts.Client(), http.MethodPost, ts.URL+tt.path, tt.body,
				token, "csrf-admin", "")
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("mutation without Idempotency-Key status=%d want 400", resp.StatusCode)
			}
		})
	}
}

func TestPhase2ProjectLifecyclePreservesReposAndOneActorPerRole(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"admin": {{ProjectID: "mail", Role: auth.HumanAdmin}},
	}, false)
	st, ts := phase2ProjectAPIServer(t, access)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "russ", Owner: "acme", Repo: "russ", Active: true}); err != nil {
		t.Fatal(err)
	}
	token := signedHumanSession(t, access, "admin", "csrf-admin")

	post := func(path string, body any, key string, want int) map[string]any {
		t.Helper()
		resp, got := humanRequest(t, ts.Client(), http.MethodPost, ts.URL+path, body,
			token, "csrf-admin", key)
		if resp.StatusCode != want {
			t.Fatalf("POST %s status=%d want=%d body=%v", path, resp.StatusCode, want, got)
		}
		return got
	}
	post("/v1/projects/mail/repos", map[string]any{"repo_id": "russ"}, "attach-russ", http.StatusNoContent)
	post("/v1/projects/mail/actors", map[string]any{
		"role": store.DriverInteractorRole, "actor_id": "interactor-mail-v1",
	}, "interactor-v1", http.StatusOK)
	post("/v1/projects/mail/actors", map[string]any{
		"role": store.DriverOrchestratorRole, "actor_id": "orchestrator-mail",
	}, "orchestrator-v1", http.StatusOK)
	post("/v1/projects/mail/actors", map[string]any{
		"role": store.DriverInteractorRole, "actor_id": "interactor-mail-v2",
	}, "interactor-v2", http.StatusOK)
	post("/v1/projects/mail/state", map[string]any{
		"state": "archived", "reason": "project complete", "expected_state_version": 1,
	}, "archive-mail", http.StatusOK)

	resp, got := humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/projects/mail", nil,
		token, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archived project detail status=%d body=%v", resp.StatusCode, got)
	}
	repos, ok := got["repository_ids"].([]any)
	if !ok || len(repos) != 1 || repos[0] != "russ" {
		t.Fatalf("archived repository history=%v", got["repository_ids"])
	}
	actors, ok := got["actors"].(map[string]any)
	if !ok || len(actors) != 2 {
		t.Fatalf("logical actor cardinality=%v", got["actors"])
	}
	interactor, ok := actors[store.DriverInteractorRole].(map[string]any)
	if !ok || interactor["actor_id"] != "interactor-mail-v2" {
		t.Fatalf("active logical interactor=%v", actors[store.DriverInteractorRole])
	}
	orchestrator, ok := actors[store.DriverOrchestratorRole].(map[string]any)
	if !ok || orchestrator["actor_id"] != "orchestrator-mail" {
		t.Fatalf("active logical orchestrator=%v", actors[store.DriverOrchestratorRole])
	}
	project := got["project"].(map[string]any)
	if project["state"] != "archived" || project["pause_reason"] != "project complete" {
		t.Fatalf("archived project state=%v", project)
	}
}

func TestPhase2DefaultProjectCompatibilityIncludesRegisteredRepos(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"legacy-admin": {{ProjectID: "default", Role: auth.HumanAdmin}},
	}, false)
	st, ts := phase2ProjectAPIServer(t, access)
	if err := st.RegisterRepo(context.Background(), store.Repo{
		ID: "legacy", Owner: "acme", Repo: "legacy", Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	token := signedHumanSession(t, access, "legacy-admin", "csrf-legacy")
	resp, got := humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/projects/default", nil,
		token, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("default project compatibility status=%d body=%v", resp.StatusCode, got)
	}
	project := got["project"].(map[string]any)
	if project["id"] != "default" {
		t.Fatalf("default project identity=%v", project)
	}
	repos := got["repository_ids"].([]any)
	if len(repos) != 1 || repos[0] != "legacy" {
		t.Fatalf("default project repository backfill=%v", repos)
	}
}

func TestPhase2ProjectManageIsForbiddenAcrossProjects(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"mail-admin": {{ProjectID: "mail", Role: auth.HumanAdmin}},
	}, false)
	st, ts := phase2ProjectAPIServer(t, access)
	for _, p := range []store.PortfolioProject{{ID: "mail", Name: "Mail"}, {ID: "calendar", Name: "Calendar"}} {
		if _, err := st.CreatePortfolioProject(context.Background(), p, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	token := signedHumanSession(t, access, "mail-admin", "csrf-mail")
	resp, _ := humanRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/projects/calendar/state",
		map[string]any{"state": "paused", "reason": "forbidden", "expected_state_version": 1},
		token, "csrf-mail", "pause-calendar")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-project manage status=%d want 403", resp.StatusCode)
	}
	calendar, err := st.GetPortfolioProject(context.Background(), "calendar")
	if err != nil || calendar.State != "active" || calendar.StateVersion != 1 {
		t.Fatalf("cross-project mutation changed state: project=%+v err=%v", calendar, err)
	}
}
