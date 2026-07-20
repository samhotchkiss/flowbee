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

func TestGlobalDashboardRendersDurableProjectFlowRiskAndThroughput(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "calendar", Name: "Calendar"}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "mail-repo", Owner: "acme", Repo: "mail", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, "mail", "mail-repo", now); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct{ id, state string }{
		{"mail-review", "awaiting_review_dispatch"}, {"mail-merge", "merge_queued"}, {"mail-done", "complete"},
	} {
		if err := st.AddEpicRun(ctx, store.EpicRun{ID: fixture.id, ProjectID: "mail", Repo: "mail-repo",
			Slug: fixture.id, Title: fixture.id, Branch: "epic/" + fixture.id}, 1, now); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET state=? WHERE epic_id=?`, fixture.state, fixture.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RecordProjectBreakerFailure(ctx, store.ProjectBreakerFailure{ProjectID: "mail", RepoID: "mail-repo",
		Kind: "github_error", Reason: "GitHub token denied", RetryAfter: time.Hour, EvidenceRef: "mail:401"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "mail-ready", ProjectID: "mail", Repo: "mail-repo",
		Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
		t.Fatal(err)
	}
	var epicSeq int
	if err := st.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(epic_seq),0)+1 FROM control_events WHERE epic_id='mail-merge'`).Scan(&epicSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO control_events
		(project_id,epic_id,kind,to_state,epic_seq,created_at) VALUES
		('mail','mail-merge','merge_verified','cleanup_pending',?,?)`, epicSeq, now.Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
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
		`data-project-id="calendar"`, `data-delivery-total="3"`, `review dispatch <b>1</b>`, `merge <b>1</b>`, `terminal <b>1</b>`,
		`data-throughput-window="24h"`, `24h throughput · 1 merged · 0 completed · 0 recovered`,
		`data-breaker-scope="repository" data-breaker-state="open"`, `mail-repo`, `GitHub token denied`,
		`why not · project breaker`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("portfolio risk/flow missing %q\n%s", want, body)
		}
	}
	if got := strings.Count(body, `data-breaker-scope="repository"`); got != 1 {
		t.Fatalf("mail breaker leaked into another project card: count=%d\n%s", got, body)
	}
}
