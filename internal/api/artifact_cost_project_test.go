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

func getArtifactCostAPI(t *testing.T, client *http.Client, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: auth.HumanSessionCookie, Value: token})
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestProjectArtifactAndCostAPIsAuthorizeAndFilterExactScope(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 23, 45, 0, 0, time.UTC)
	st := testutil.NewStore(t)
	for _, projectID := range []string{"alpha", "beta"} {
		if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: projectID, Name: projectID}, now); err != nil {
			t.Fatal(err)
		}
		repoID := "repo-" + projectID
		if err := st.RegisterRepo(ctx, store.Repo{ID: repoID, Owner: "fixture", Repo: repoID, Active: true}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddProjectRepo(ctx, projectID, repoID, now); err != nil {
			t.Fatal(err)
		}
		if err := st.AddEpicRun(ctx, store.EpicRun{ID: projectID + "-epic", ProjectID: projectID,
			Slug: "same-label", AdmissionKey: projectID + ":epic", Repo: repoID, Branch: "epic/same-label"}, 1, now); err != nil {
			t.Fatal(err)
		}
		if err := st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{EpicID: projectID + "-epic",
			ProjectID: projectID, Repo: repoID, Branch: "epic/same-label", PRNumber: 9,
			PROpen: true, HeadSHA: "head-" + projectID, BaseSHA: "base", CIState: "pending"}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SeedJob(ctx, store.SeedParams{ID: projectID + "-job", ProjectID: projectID,
			Kind: job.KindBuild, Flow: "build", FlowID: "same-label", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
			t.Fatal(err)
		}
		claimed, err := st.ClaimReadyJob(ctx, store.ClaimParams{JobID: projectID + "-job",
			LeaseID: "lease-" + projectID, Identity: "worker-" + projectID, ModelFamily: "codex",
			Role: job.RoleEngWorker, Attested: []string{"role:eng_worker", "model_family:codex"},
			TTL: time.Hour, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.RecordCost(ctx, store.CostParams{JobID: projectID + "-job", ProjectID: projectID,
			Epoch: claimed.Epoch, Now: now, MicroUSDDelta: 10}); err != nil {
			t.Fatal(err)
		}
	}

	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"alpha-viewer":     {{ProjectID: "alpha", Role: auth.HumanViewer}},
		"portfolio-viewer": {{ProjectID: "*", Role: auth.HumanViewer}},
	}, false)
	srv := api.New(st, clock.NewFake(now), ulid.NewMinter(nil), api.Config{HumanAccess: access}, "artifact-cost-api")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	alphaToken := signedHumanSession(t, access, "alpha-viewer", "csrf-alpha")
	portfolioToken := signedHumanSession(t, access, "portfolio-viewer", "csrf-portfolio")

	resp := getArtifactCostAPI(t, ts.Client(), ts.URL+"/v1/cost?project_id=alpha", alphaToken)
	defer resp.Body.Close()
	var alphaCosts []store.FlowCostRow
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&alphaCosts) != nil ||
		len(alphaCosts) != 1 || alphaCosts[0].ProjectID != "alpha" || alphaCosts[0].JobID != "alpha-job" {
		t.Fatalf("alpha cost status=%d rows=%+v", resp.StatusCode, alphaCosts)
	}
	for _, path := range []string{"/v1/cost?project_id=beta", "/v1/cost"} {
		denied := getArtifactCostAPI(t, ts.Client(), ts.URL+path, alphaToken)
		denied.Body.Close()
		if denied.StatusCode != http.StatusForbidden {
			t.Fatalf("project grant widened path=%s status=%d", path, denied.StatusCode)
		}
	}
	portfolio := getArtifactCostAPI(t, ts.Client(), ts.URL+"/v1/cost", portfolioToken)
	defer portfolio.Body.Close()
	var allCosts []store.FlowCostRow
	if portfolio.StatusCode != http.StatusOK || json.NewDecoder(portfolio.Body).Decode(&allCosts) != nil || len(allCosts) != 2 {
		t.Fatalf("portfolio cost status=%d rows=%+v", portfolio.StatusCode, allCosts)
	}

	epics := getArtifactCostAPI(t, ts.Client(), ts.URL+"/v1/projects/alpha/epics", alphaToken)
	defer epics.Body.Close()
	var body struct {
		ProjectID string                   `json:"project_id"`
		Artifacts []store.EpicArtifactView `json:"artifacts"`
	}
	if epics.StatusCode != http.StatusOK || json.NewDecoder(epics.Body).Decode(&body) != nil ||
		body.ProjectID != "alpha" || len(body.Artifacts) != 1 || body.Artifacts[0].EpicID != "alpha-epic" ||
		body.Artifacts[0].HeadSHA != "head-alpha" {
		t.Fatalf("project epics status=%d body=%+v", epics.StatusCode, body)
	}
	deniedArtifacts := getArtifactCostAPI(t, ts.Client(), ts.URL+"/v1/projects/beta/epics", alphaToken)
	deniedArtifacts.Body.Close()
	if deniedArtifacts.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-project artifact status=%d want 403", deniedArtifacts.StatusCode)
	}
}
