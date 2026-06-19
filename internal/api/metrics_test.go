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

// TestMetricsOutboxAbandoned: dead-lettered GitHub writes surface as a per-action gauge so an
// operator can alert on silently-dropped work; with none abandoned, the series is absent.
func TestMetricsOutboxAbandoned(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{}, "v")
	get := func() string {
		rec := httptest.NewRecorder()
		srv.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}
	if strings.Contains(get(), "flowbee_outbox_abandoned") {
		t.Fatal("no abandoned actions => the series must be absent")
	}
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO outbox (job_id, action, head_sha, status) VALUES ('j','issues.create','h1','abandoned'),('k','issues.create','h2','abandoned'),('m','pulls.comment','h3','abandoned')`); err != nil {
		t.Fatal(err)
	}
	body := get()
	if !strings.Contains(body, `flowbee_outbox_abandoned{action="issues.create"} 2`) {
		t.Errorf("want issues.create=2:\n%s", body)
	}
	if !strings.Contains(body, `flowbee_outbox_abandoned{action="pulls.comment"} 1`) {
		t.Errorf("want pulls.comment=1:\n%s", body)
	}
}

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
