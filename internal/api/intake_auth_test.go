package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// TestIntakeAndOperatorEdgesRequireAuth: under the secure (non-loopback, authenticator
// configured) posture, the WRITE / intake edges — POST /v1/specs, /v1/epics, and the
// operator job mutations (cancel/requeue/promote/adopt/design) — must reject an
// unauthenticated off-loopback caller, so no one who can merely reach :7070 can inject
// work or mutate jobs without a credential. (Regression: these previously sat on the bare
// mux, open even with worker_auth_secret set.) Read-only dashboard endpoints stay open.
func TestIntakeAndOperatorEdgesRequireAuth(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}

	// authenticator with a secret + an enrolled identity, NO loopback bypass — the secure
	// non-loopback production posture.
	authn := auth.NewBearer([]byte("server-secret"), []string{"planner"}, false)
	srv := api.New(st, clock.NewFake(time.Unix(1000, 0)), ulid.NewMinter(nil),
		api.Config{Authenticator: authn}, "v")
	h := srv.PrivateHandler()

	writeEdges := []struct{ method, path string }{
		{http.MethodPost, "/v1/specs"},
		{http.MethodPost, "/v1/epics"},
		{http.MethodPost, "/v1/jobs/j/cancel"},
		{http.MethodPost, "/v1/jobs/j/requeue"},
		{http.MethodPost, "/v1/jobs/j/promote"},
		{http.MethodPost, "/v1/jobs/j/adopt"},
		{http.MethodPost, "/v1/jobs/j/design"},
	}

	// off-loopback (httptest default RemoteAddr is TEST-NET, non-loopback), no token => 401.
	for _, e := range writeEdges {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(e.method, e.path, strings.NewReader(`{}`))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: status=%d, want 401", e.method, e.path, rec.Code)
		}
	}

	// a valid token gets PAST auth — the intake endpoint no longer 401s (it processes the
	// request; a 2xx/4xx-business response, anything but 401, proves auth passed).
	tok := authn.Mint("planner")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/specs",
		strings.NewReader(`{"task":"x","acceptance":"y"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("POST /v1/specs WITH a valid token still 401'd: %s", rec.Body.String())
	}

	// a read-only dashboard endpoint stays OPEN (no token) — the UI/feed must keep working.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/board", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Errorf("GET /v1/board must remain open (read-only dashboard), got 401")
	}
}
