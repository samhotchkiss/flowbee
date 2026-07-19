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

func projectBreakerAPIServer(t *testing.T, access *auth.HumanAccess) (*store.Store, *httptest.Server) {
	t.Helper()
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{HumanAccess: access}, "breaker-api-test")
	ts := httptest.NewServer(srv.ProjectCircuitBreakerHandler())
	t.Cleanup(ts.Close)
	return st, ts
}

func seedProjectBreakerAPIProject(t *testing.T, st *store.Store, projectID, repoID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: projectID, Name: projectID}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if repoID == "" {
		return
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: repoID, Owner: "acme", Repo: repoID, Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, projectID, repoID, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestProjectCircuitBreakerQueryIsAuthenticatedAndProjectScoped(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"alpha-viewer": {{ProjectID: "alpha", Role: auth.HumanViewer}},
	}, false)
	st, ts := projectBreakerAPIServer(t, access)
	seedProjectBreakerAPIProject(t, st, "alpha", "repo-a")
	seedProjectBreakerAPIProject(t, st, "beta", "repo-b")
	if _, err := st.RecordProjectBreakerFailure(context.Background(), store.ProjectBreakerFailure{
		ProjectID: "alpha", RepoID: "repo-a", Kind: "ci_outage", Reason: "checks unavailable",
		RetryAfter: time.Minute, EvidenceRef: "github:check:42",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	resp, _ := humanRequest(t, ts.Client(), http.MethodGet,
		ts.URL+"/v1/projects/alpha/circuit-breakers", nil, "", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous query status=%d want 401", resp.StatusCode)
	}
	token := signedHumanSession(t, access, "alpha-viewer", "csrf-alpha")
	resp, body := humanRequest(t, ts.Client(), http.MethodGet,
		ts.URL+"/v1/projects/alpha/circuit-breakers", nil, token, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("project query status=%d body=%v", resp.StatusCode, body)
	}
	rows, ok := body["breakers"].([]any)
	if !ok || len(rows) != 1 || rows[0].(map[string]any)["repo_id"] != "repo-a" {
		t.Fatalf("project breakers=%v", body["breakers"])
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodGet,
		ts.URL+"/v1/projects/beta/circuit-breakers", nil, token, "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-project query status=%d want 403", resp.StatusCode)
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/projects/alpha/circuit-breakers/open",
		map[string]any{"reason": "operator hold"}, token, "csrf-alpha", "viewer-open")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer mutation status=%d want 403", resp.StatusCode)
	}
}

func TestProjectCircuitBreakerControlsAreFencedIdempotentAndAudited(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"sam": {{ProjectID: "alpha", Role: auth.HumanAdmin}},
	}, false)
	st, ts := projectBreakerAPIServer(t, access)
	seedProjectBreakerAPIProject(t, st, "alpha", "repo-a")
	token := signedHumanSession(t, access, "sam", "csrf-alpha")
	path := ts.URL + "/v1/projects/alpha/circuit-breakers/open"
	body := map[string]any{
		"repo_id": "repo-a", "expected_state_version": 0, "reason": "credential rotation",
		"failure_kind": "github_error", "probe_after_seconds": 600,
	}

	resp, _ := humanRequest(t, ts.Client(), http.MethodPost, path, body, token, "", "open-repo-a")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d want 403", resp.StatusCode)
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost, path, body, token, "csrf-alpha", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing idempotency status=%d want 400", resp.StatusCode)
	}
	resp, result := humanRequest(t, ts.Client(), http.MethodPost, path, body, token, "csrf-alpha", "open-repo-a")
	if resp.StatusCode != http.StatusOK || result["breaker"].(map[string]any)["state_version"] != float64(1) {
		t.Fatalf("open status=%d body=%v", resp.StatusCode, result)
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost, path, body, token, "csrf-alpha", "open-repo-a")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exact replay status=%d want 200", resp.StatusCode)
	}
	changed := map[string]any{
		"repo_id": "repo-a", "expected_state_version": 0, "reason": "different hold",
		"failure_kind": "github_error", "probe_after_seconds": 600,
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost, path, changed, token, "csrf-alpha", "open-repo-a")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("changed replay status=%d want 409", resp.StatusCode)
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost, path, body, token, "csrf-alpha", "stale-open")
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale open status=%d want 412", resp.StatusCode)
	}

	probePath := ts.URL + "/v1/projects/alpha/circuit-breakers/probe-now"
	probe := map[string]any{"repo_id": "repo-a", "expected_state_version": 0, "reason": "credential rotated"}
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost, probePath, probe, token, "csrf-alpha", "stale-probe")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("zero-version probe status=%d want 400", resp.StatusCode)
	}
	probe["expected_state_version"] = 1
	resp, result = humanRequest(t, ts.Client(), http.MethodPost, probePath, probe, token, "csrf-alpha", "probe-repo-a")
	if resp.StatusCode != http.StatusOK || result["breaker"].(map[string]any)["state_version"] != float64(2) {
		t.Fatalf("probe-now status=%d body=%v", resp.StatusCode, result)
	}
	stale := map[string]any{"repo_id": "repo-a", "expected_state_version": 1, "reason": "again"}
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost, probePath, stale, token, "csrf-alpha", "stale-version")
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale version status=%d want 412", resp.StatusCode)
	}

	events, err := st.ListProjectBreakerEvents(context.Background(), "alpha", "repo-a")
	if err != nil || len(events) != 2 || events[0].ActorID != "sam" || events[1].Kind != "operator_probe_requested" {
		t.Fatalf("operator audit=%+v err=%v", events, err)
	}
	var commandCount int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM project_circuit_breaker_commands
		WHERE project_id='alpha'`).Scan(&commandCount); err != nil || commandCount != 2 {
		t.Fatalf("durable commands=%d err=%v", commandCount, err)
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/projects/alpha/circuit-breakers/close", map[string]any{}, token, "csrf-alpha", "close")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("force-close route status=%d want 404", resp.StatusCode)
	}
}
