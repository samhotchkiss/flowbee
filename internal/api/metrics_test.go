package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// TestMetricsEndpoint: GET /metrics on the health handler returns Prometheus
// text-format output with the baseline operational series, unauthenticated.
func TestMetricsEndpoint(t *testing.T) {
	ctx := context.Background()
	dsn := t.TempDir() + "/flowbee.db"
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{}, "test-version")

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.HealthHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain prefix", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`flowbee_build_info{version="test-version"} 1`,
		"# TYPE flowbee_jobs gauge",
		`flowbee_fleet_workers{status="live"} 0`,
		`flowbee_fleet_workers{status="stale"} 0`,
		"flowbee_fleet_waiting_jobs 0",
		"flowbee_cost_micro_usd_total 0",
		"flowbee_jobs_over_budget 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q\n--- body ---\n%s", want, body)
		}
	}
}
