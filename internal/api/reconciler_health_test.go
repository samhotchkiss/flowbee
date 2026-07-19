package api_test

import (
	"context"
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

func TestHealthEndpointIsExternalReconcilerDeadMan(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 17, 0, 0, 0, time.UTC)
	if _, err := st.BeginReconciler(ctx, "reconciler_watchdog", "serve-1", now, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(now)
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{}, "v2")

	request := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := request("/healthz"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"reconciler_overdue":0`) {
		t.Fatalf("fresh health code=%d body=%s", rec.Code, rec.Body.String())
	}
	clk.Advance(20 * time.Second)
	if rec := request("/healthz"); rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), `"reconciler_overdue":1`) ||
		!strings.Contains(rec.Body.String(), "reconciler_watchdog") {
		t.Fatalf("overdue health code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request("/metrics"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "flowbee_reconciler_overdue 1") {
		t.Fatalf("dead-man metric code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHealthEndpointFailsClosedWhenDriverControlOriginUnavailable(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	srv := api.New(st, clock.NewFake(time.Unix(1000, 0)), ulid.NewMinter(nil), api.Config{
		DriverControl: api.DriverControlReadiness{
			Required: true, Status: "route_unavailable", Gap: "GAP-FD-003",
			Reason: "authenticated Flowbee control origin is unsupported",
		},
	}, "v2")

	rec := httptest.NewRecorder()
	srv.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health code=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"status":"degraded"`, `"available":false`,
		`"status":"route_unavailable"`, `"gap":"GAP-FD-003"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("health missing %s: %s", want, rec.Body.String())
		}
	}
}

func TestHealthEndpointReadsLiveDriverControlState(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	current := api.DriverControlReadiness{Required: true, Available: true, Status: "ready"}
	srv := api.New(st, clock.NewFake(time.Unix(1000, 0)), ulid.NewMinter(nil), api.Config{
		DriverControl:        current,
		DriverControlCurrent: func() api.DriverControlReadiness { return current },
	}, "v2")
	request := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rec
	}
	if rec := request(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"available":true`) {
		t.Fatalf("ready health code=%d body=%s", rec.Code, rec.Body.String())
	}
	current = api.DriverControlReadiness{Required: true, Status: "route_unavailable", Gap: "GAP-FD-003", Reason: "token revoked"}
	if rec := request(); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "token revoked") {
		t.Fatalf("revoked health code=%d body=%s", rec.Code, rec.Body.String())
	}
	current = api.DriverControlReadiness{Required: true, Available: true, Status: "ready"}
	if rec := request(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"available":true`) {
		t.Fatalf("restored health code=%d body=%s", rec.Code, rec.Body.String())
	}
}
