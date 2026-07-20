package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func TestBootstrapOnlyReadinessKeepsFinalHealthAndLeaseDispatchClosed(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	dispatch := false
	srv := New(st, clock.NewFake(time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)),
		ulid.NewMinter(nil), Config{BootstrapProjectID: "russ",
			BootstrapDispatchAllowed: func() bool { return dispatch },
			Phase1ProjectCurrent: func() Phase1ProjectReadiness {
				return Phase1ProjectReadiness{Required: true, ProjectID: "russ", Status: "held",
					Holds: []string{"missing_interactor_route"}}
			}}, "p1")
	srv.SetBootstrapActionIntake(&bootstrapIntakeFake{})

	bootstrapRec := httptest.NewRecorder()
	srv.HealthHandler().ServeHTTP(bootstrapRec, httptest.NewRequest(http.MethodGet, "/bootstrapz", nil))
	if bootstrapRec.Code != http.StatusOK ||
		!strings.Contains(bootstrapRec.Body.String(), `"status":"bootstrap_ready"`) ||
		!strings.Contains(bootstrapRec.Body.String(), `"dispatch_allowed":false`) {
		t.Fatalf("bootstrap readiness code=%d body=%s", bootstrapRec.Code, bootstrapRec.Body.String())
	}
	healthRec := httptest.NewRecorder()
	srv.HealthHandler().ServeHTTP(healthRec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if healthRec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(healthRec.Body.String(), `"status":"degraded"`) {
		t.Fatalf("final health code=%d body=%s", healthRec.Code, healthRec.Body.String())
	}

	leaseRec := httptest.NewRecorder()
	srv.lease(leaseRec, httptest.NewRequest(http.MethodGet, "/v1/lease?identity=worker-a", nil))
	if leaseRec.Code != http.StatusNoContent {
		t.Fatalf("bootstrap-held lease status=%d body=%s", leaseRec.Code, leaseRec.Body.String())
	}
	var workers int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM workers`).Scan(&workers); err != nil || workers != 0 {
		t.Fatalf("bootstrap-held lease mutated worker state count=%d err=%v", workers, err)
	}

	// Opening the bootstrap gate does not clear or mutate the operator pause
	// controls; it only removes this independent pre-activation fence.
	dispatch = true
	if paused, err := st.DispatchPaused(ctx); err != nil || paused {
		t.Fatalf("bootstrap gate changed operator pause state paused=%v err=%v", paused, err)
	}
}
