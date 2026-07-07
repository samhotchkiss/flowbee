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

// TestSpecCreateRepoLessWithMultipleReposIsRejected is the regression for a real
// incident: a /v1/specs ingest with no "repo" used to silently default to "the
// primary registered repo (first by id)". Three raw context-dump specs about the
// `russ` mail product (issues #254, #257, #258) were POSTed without a `repo` field
// and silently landed in flowbee's OWN pipeline instead — built, reviewed, and
// bounced there for days before anyone noticed, since every eng_worker/reviewer
// correctly found nothing in flowbee's repo matching a spec about russ's
// backend/internal/email paths. With two or more repos registered, a repo-less
// ingest must now be a hard 400 rejection instead of a silent guess.
func TestSpecCreateRepoLessWithMultipleReposIsRejected(t *testing.T) {
	ctx := context.Background()
	st, srv := newSpecServer(t)
	for _, r := range []store.Repo{
		{ID: "zrepo", Owner: "o", Repo: "z", DefaultBranch: "main", Active: true},
		{ID: "arepo", Owner: "o", Repo: "a", DefaultBranch: "main", Active: true},
	} {
		if err := st.RegisterRepo(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/specs", strings.NewReader(`{"task":"add request timeouts"}`))
	srv.PrivateHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("repo-less spec with 2 registered repos: status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "arepo") || !strings.Contains(rec.Body.String(), "zrepo") {
		t.Fatalf("400 body should name the registered repos so the caller can pick one; got %q", rec.Body.String())
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

// TestEpicCreateRepoLessDefaultsToPrimaryRepo: a /v1/epics ingest with no "repo" must
// also land on the primary registered repo (epicCreate shares the same defaulting as
// specCreate), so the epic barrier + its fanned-out children are drained, not stranded.
func TestEpicCreateRepoLessDefaultsToPrimaryRepo(t *testing.T) {
	ctx := context.Background()
	st, srv := newSpecServer(t)
	if err := st.RegisterRepo(ctx, store.Repo{ID: "arepo", Owner: "o", Repo: "a", DefaultBranch: "main", Active: true}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/epics",
		strings.NewReader(`{"title":"e","issues":[{"task":"a"},{"task":"b"}]}`))
	srv.PrivateHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/epics status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		EpicID string `json:"epic_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	j, err := st.GetJob(ctx, resp.EpicID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Repo != "arepo" {
		t.Fatalf("repo-less epic repo=%q, want the primary registered repo \"arepo\" (NOT \"default\")", j.Repo)
	}
}

// TestEpicCreateRepoLessWithMultipleReposIsRejected mirrors
// TestSpecCreateRepoLessWithMultipleReposIsRejected for /v1/epics, which shares the
// same resolveIngestRepo call.
func TestEpicCreateRepoLessWithMultipleReposIsRejected(t *testing.T) {
	ctx := context.Background()
	st, srv := newSpecServer(t)
	for _, r := range []store.Repo{
		{ID: "zrepo", Owner: "o", Repo: "z", DefaultBranch: "main", Active: true},
		{ID: "arepo", Owner: "o", Repo: "a", DefaultBranch: "main", Active: true},
	} {
		if err := st.RegisterRepo(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/epics",
		strings.NewReader(`{"title":"e","issues":[{"task":"a"},{"task":"b"}]}`))
	srv.PrivateHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("repo-less epic with 2 registered repos: status=%d, want 400; body=%s", rec.Code, rec.Body.String())
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
