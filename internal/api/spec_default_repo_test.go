package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func newSpecServer(t *testing.T) (*store.Store, *api.Server) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	return st, api.New(st, clock.NewFake(time.Unix(1000, 0)), ulid.NewMinter(nil), api.Config{}, "v")
}

func postSpec(t *testing.T, srv *api.Server, jsonBody string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/specs", strings.NewReader(jsonBody))
	srv.PrivateHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/specs status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.JobID
}

// TestSpecCreateRepoLessDefaultsToPrimaryRepo: a /v1/specs ingest with no "repo" must
// land on the PRIMARY registered repo (first by id) — a real, project-out-drained repo —
// not the literal "default", which no per-repo drain covers (so the materialization
// outbox row, and the whole spec flow after sign-off, would strand forever).
func TestSpecCreateRepoLessDefaultsToPrimaryRepo(t *testing.T) {
	ctx := context.Background()
	st, srv := newSpecServer(t)
	// register two repos; the primary is the first by id ("arepo" < "zrepo").
	for _, r := range []store.Repo{
		{ID: "zrepo", Owner: "o", Repo: "z", DefaultBranch: "main", Active: true},
		{ID: "arepo", Owner: "o", Repo: "a", DefaultBranch: "main", Active: true},
	} {
		if err := st.RegisterRepo(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	jobID := postSpec(t, srv, `{"task":"add request timeouts"}`)
	j, err := st.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Repo != "arepo" {
		t.Fatalf("repo-less spec repo=%q, want the primary registered repo \"arepo\" (NOT \"default\")", j.Repo)
	}
}

// TestSpecCreateRepoLessNoReposFallsBackToEmpty: with no repos registered (legacy
// single-repo), a repo-less ingest falls back to the "" scope the non-repo-scoped sender
// drains — never the un-drained literal "default".
func TestSpecCreateRepoLessNoReposFallsBackToEmpty(t *testing.T) {
	ctx := context.Background()
	st, srv := newSpecServer(t)
	jobID := postSpec(t, srv, `{"task":"x"}`)
	j, err := st.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Repo != "" {
		t.Fatalf("with no registered repos, repo-less spec repo=%q, want legacy \"\" scope", j.Repo)
	}
}

// TestSpecCreateHonorsExplicitRepo: an explicit "repo" is used verbatim.
func TestSpecCreateHonorsExplicitRepo(t *testing.T) {
	ctx := context.Background()
	st, srv := newSpecServer(t)
	if err := st.RegisterRepo(ctx, store.Repo{ID: "arepo", Owner: "o", Repo: "a", DefaultBranch: "main", Active: true}); err != nil {
		t.Fatal(err)
	}
	jobID := postSpec(t, srv, `{"task":"x","repo":"arepo"}`)
	j, _ := st.GetJob(ctx, jobID)
	if j.Repo != "arepo" {
		t.Fatalf("explicit repo not honored: got %q", j.Repo)
	}
}
