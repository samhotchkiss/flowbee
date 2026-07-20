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

func TestHealthAndMetricsFenceSupersededControlPlaneIncarnation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if err := st.AcquireWriterLock(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 4, 0, 0, 0, time.UTC)
	const posture = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const firstID = "cpi-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const secondID = "cpi-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	first := store.StartControlPlaneIncarnationInput{ID: firstID, Version: "v2-first",
		SourceCommit: "first-commit", ConfigPostureSHA256: posture, ProcessID: 1, StartedAt: now}
	if _, err := st.StartControlPlaneIncarnation(ctx, first); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(now)
	serverFor := func(id, version string) *api.Server {
		return api.New(st, clk, ulid.NewMinter(nil), api.Config{
			ControlPlaneIncarnationID: id,
		}, version)
	}
	request := func(srv *api.Server, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	oldServer := serverFor(first.ID, first.Version)
	if rec := request(oldServer, "/healthz"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"incarnation_id":"`+firstID+`"`) {
		t.Fatalf("first health code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(oldServer, "/metrics"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `incarnation_id="`+firstID+`"`) ||
		!strings.Contains(rec.Body.String(), `flowbee_control_plane_incarnation_current{expected_incarnation_id="`+firstID+`"} 1`) {
		t.Fatalf("first metrics code=%d body=%s", rec.Code, rec.Body.String())
	}

	second := store.StartControlPlaneIncarnationInput{ID: secondID, Version: "v2-second",
		SourceCommit: "second-commit", ConfigPostureSHA256: posture, ProcessID: 2,
		StartedAt: now.Add(time.Minute)}
	if _, err := st.StartControlPlaneIncarnation(ctx, second); err != nil {
		t.Fatal(err)
	}
	if rec := request(oldServer, "/healthz"); rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), `"control_plane_incarnation_expected":"`+firstID+`"`) ||
		!strings.Contains(rec.Body.String(), `"incarnation_id":"`+secondID+`"`) {
		t.Fatalf("superseded health code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(oldServer, "/metrics"); !strings.Contains(rec.Body.String(),
		`flowbee_control_plane_incarnation_current{expected_incarnation_id="`+firstID+`"} 0`) {
		t.Fatalf("superseded metrics=%s", rec.Body.String())
	}
	replacement := serverFor(second.ID, second.Version)
	if rec := request(replacement, "/healthz"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"incarnation_id":"`+secondID+`"`) {
		t.Fatalf("replacement health code=%d body=%s", rec.Code, rec.Body.String())
	}
}
