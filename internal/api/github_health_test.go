package api_test

import (
	"context"
	"errors"
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

// TestGitHubSweepHealthSignal: a sustained reconcile failure (an expired/revoked token,
// rate-limit, connectivity) must be OBSERVABLE — flowbee_github_last_success_age_seconds
// grows and /healthz carries the error — instead of silently logging every 45s. A
// successful sweep resets the signal.
func TestGitHubSweepHealthSignal(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Unix(1000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{}, "v")

	body := func(path string) string {
		rec := httptest.NewRecorder()
		srv.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Body.String()
	}

	// seeded at construction: age 0. Now a sustained failure that does NOT advance the
	// last-success watermark, so the age grows as time passes.
	srv.RecordGitHubSweep(errors.New("rest GET /repos: 401: Bad credentials"))
	clk.Advance(5 * time.Minute)

	if m := body("/metrics"); !strings.Contains(m, "flowbee_github_last_success_age_seconds 300") {
		t.Fatalf("metrics must show the GitHub age growing (300s): %s", m)
	}
	if hz := body("/healthz"); !strings.Contains(hz, "401: Bad credentials") ||
		!strings.Contains(hz, `"github_last_success_age_seconds":300`) {
		t.Fatalf("healthz must surface the GitHub error + age: %s", hz)
	}

	// recovery: a successful sweep advances the watermark -> age resets, error clears.
	srv.RecordGitHubSweep(nil)
	if m := body("/metrics"); !strings.Contains(m, "flowbee_github_last_success_age_seconds 0") {
		t.Fatalf("after a successful sweep the age must reset to 0: %s", m)
	}
	if hz := body("/healthz"); strings.Contains(hz, "github_last_error") {
		t.Fatalf("healthz must clear the error after recovery: %s", hz)
	}
}
