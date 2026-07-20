package web_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

func TestGlobalDashboardRendersProjectPortfolioAndNeedsYouProvenanceLink(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 18, 30, 0, 0, time.UTC)
	project, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{
		ID: "mail", Name: "Mail control plane",
		Priority: 7, SchedulerWeight: 4, ConcurrencyCap: 3,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetPortfolioProjectState(ctx, "mail", "paused", "release hold", project.StateVersion, now); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("mail-design"))
	if _, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
		ID: "mail-design", ProjectID: "mail", Kind: workintent.DecisionDesignReview,
		Title: "Review mail design", Prompt: "Approve the design?", Options: json.RawMessage(`[]`),
		ResponseSchema: json.RawMessage(`{}`), ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseApprove},
		RequestedBy: "orchestrator:mail", RouteTo: "human:sam", SubjectArtifactRef: "artifact://mail-design",
		SubjectVersion: 1, SubjectSHA256: "sha256:" + hex.EncodeToString(hash[:]),
	}, now); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard", nil)
	mountUI(t, st, fixedClock{t: now.Add(time.Hour)}).ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`data-project-portfolio`, `Mail control plane`, `priority 7`, `weight 4`, `cap 3`,
		`href="/workspace?project=mail"`, `Review mail design`, `data-project-id="mail"`,
		`Interactor · unregistered`, `Orchestrator · unregistered`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("portfolio missing %q\n%s", want, body)
		}
	}
}

func TestGlobalDashboardMakesProjectStarvationAndFairnessExplicit(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{
		ID: "mail", Name: "Mail", SchedulerWeight: 3,
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "mail-starved", Kind: job.KindBuild,
		Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now.Add(-20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET project_id='mail' WHERE id='mail-starved'`); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard", nil)
	mountUI(t, st, fixedClock{t: now}).ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`data-project-id="mail"`, `data-pool="build" data-starved="true"`,
		`0 allocated`, `service 0/0 · 0.00%`, `weight share 75.00%`,
		`1 eligible · oldest 20m`, `starvation bound exceeded`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("portfolio fairness missing %q\n%s", want, body)
		}
	}
}
