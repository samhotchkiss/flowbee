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

func TestRunningConfigEndpointRequiresAuthAndIsRedacted(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	authn := auth.NewBearer([]byte("server-secret"), []string{"worker"}, false)
	behind := 2
	srv := api.New(st, clock.NewFake(time.Unix(1000, 0)), ulid.NewMinter(nil), api.Config{
		Authenticator: authn,
		RunningConfig: api.RunningConfig{
			SourceCommit:         "0cef2e5ac1f6aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			TreeDirty:            true,
			BehindOriginMainBy:   &behind,
			OriginMainWarning:    "WARN: running binary is 2 commits behind origin/main (built from 0cef2e5ac1f6, dirty=true) - merged fixes may be missing",
			ConfigPath:           "/home/sam/.flowbee/flowbee.yaml",
			DatabaseURL:          "/home/sam/.flowbee/flowbee.db",
			PrivateAddr:          ":7070",
			AllowSelfMerge:       true,
			GitHubTokenPresent:   true,
			WorkerAuthConfigured: true,
			Repos: []api.RunningConfigRepo{{
				ID: "flowbee", Owner: "samhotchkiss", Repo: "flowbee", TokenPresent: true,
			}},
		},
	}, "test-version")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	srv.PrivateHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /v1/config status=%d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	req.Header.Set("Authorization", "Bearer "+authn.Mint("worker"))
	srv.PrivateHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/config status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"version":"test-version"`, `"github_token_present":true`, `"worker_auth_configured":true`, `"allow_self_merge":true`,
		`"source_commit":"0cef2e5ac1f6aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, `"tree_dirty":true`, `"behind_origin_main_by":2`,
		`"origin_main_warning":"WARN: running binary is 2 commits behind origin/main`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("running config missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, "server-secret") || strings.Contains(body, "github_pat_") {
		t.Fatalf("running config must not expose secret values: %s", body)
	}
}

func TestRunningConfigOpenAPIIsLoopbackOnly(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	srv := api.New(st, clock.NewFake(time.Unix(1000, 0)), ulid.NewMinter(nil), api.Config{
		RunningConfig: api.RunningConfig{PrivateAddr: ":7070", InsecureWorkerAPI: true},
	}, "test-version")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	req.RemoteAddr = "100.64.0.2:12345"
	srv.PrivateHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("off-loopback open API GET /v1/config status=%d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	srv.PrivateHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback open API GET /v1/config status=%d body=%s", rec.Code, rec.Body.String())
	}
}
