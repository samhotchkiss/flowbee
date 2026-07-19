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
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

func TestHumanDecisionAuditExportIsExactProjectScoped(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO projects(id,name,state,created_at,updated_at)
		VALUES ('other','Other','active',?,?)`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	create := func(projectID, id, hash string) {
		t.Helper()
		_, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
			ID: id, ProjectID: projectID, Kind: workintent.DecisionPlanReview,
			Title: "Review " + id, Prompt: "Review the exact project artifact.",
			ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseApprove},
			RequestedBy:           "interactor:" + projectID, RouteTo: "human:sam",
			SubjectArtifactRef: "artifact://" + id, SubjectVersion: 1, SubjectSHA256: hash,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	create("default", "default-audit", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	create("other", "other-audit", "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"sam": {{ProjectID: "default", Role: auth.HumanViewer}},
	}, false)
	srv := api.New(st, clock.NewFake(now), ulid.NewMinter(nil), api.Config{HumanAccess: access}, "audit-test")
	ts := httptest.NewServer(srv.HumanDecisionAuditHandler())
	t.Cleanup(ts.Close)
	token := signedHumanSession(t, access, "sam", "csrf-audit")

	get := func(path string) (*http.Response, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.AddCookie(&http.Cookie{Name: auth.HumanSessionCookie, Value: token})
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp, body
	}

	resp, body := get("?project_id=default")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("default audit status=%d body=%v", resp.StatusCode, body)
	}
	rows, _ := body["decisions"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["request"].(map[string]any)["id"] != "default-audit" {
		t.Fatalf("project audit leaked or omitted rows: %v", body)
	}
	resp, _ = get("?project_id=other")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other project audit status=%d want 403", resp.StatusCode)
	}
	resp, _ = get("")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unscoped audit status=%d want 400", resp.StatusCode)
	}
}
