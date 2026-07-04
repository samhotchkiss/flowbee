package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func TestBoardTraceRejectsSpoofedRoleHeaderAtServer(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "trace-1", Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, TaskText: "Trace this card", Now: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	authn := auth.NewBearer([]byte("server-secret"), []string{"admin", "viewer"}, false)
	srv := api.New(st, clock.NewFake(now), ulid.NewMinter(nil), api.Config{
		Authenticator:        authn,
		SuperadminIdentities: []string{"admin"},
	}, "v")
	h := srv.PrivateHandler()

	req := httptest.NewRequest(http.MethodGet, "/board/trace?job=trace-1", nil)
	req.Header.Set("X-Flowbee-Role", "superadmin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed role header status=%d, want 401; body:\n%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/board/trace?job=trace-1", nil)
	req.Header.Set("Authorization", "Bearer "+authn.Mint("viewer"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("authenticated non-superadmin status=%d, want 403; body:\n%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/board/trace?job=trace-1", nil)
	req.Header.Set("Authorization", "Bearer "+authn.Mint("admin"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated superadmin status=%d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
}
