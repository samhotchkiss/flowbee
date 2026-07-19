package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

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
